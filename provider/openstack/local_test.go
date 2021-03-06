// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package openstack_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	jujuerrors "github.com/juju/errors"
	gitjujutesting "github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils"
	"github.com/juju/utils/arch"
	"github.com/juju/utils/series"
	"github.com/juju/utils/ssh"
	"github.com/juju/version"
	gc "gopkg.in/check.v1"
	"gopkg.in/goose.v1/client"
	"gopkg.in/goose.v1/identity"
	"gopkg.in/goose.v1/nova"
	"gopkg.in/goose.v1/testservices/hook"
	"gopkg.in/goose.v1/testservices/identityservice"
	"gopkg.in/goose.v1/testservices/novaservice"
	"gopkg.in/goose.v1/testservices/openstackservice"

	"github.com/juju/juju/cloud"
	"github.com/juju/juju/cloudconfig/instancecfg"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/bootstrap"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/filestorage"
	"github.com/juju/juju/environs/imagemetadata"
	imagetesting "github.com/juju/juju/environs/imagemetadata/testing"
	"github.com/juju/juju/environs/jujutest"
	"github.com/juju/juju/environs/simplestreams"
	sstesting "github.com/juju/juju/environs/simplestreams/testing"
	"github.com/juju/juju/environs/storage"
	envtesting "github.com/juju/juju/environs/testing"
	"github.com/juju/juju/environs/tools"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/juju/keys"
	"github.com/juju/juju/juju/testing"
	"github.com/juju/juju/jujuclient/jujuclienttesting"
	"github.com/juju/juju/network"
	"github.com/juju/juju/provider/common"
	"github.com/juju/juju/provider/openstack"
	"github.com/juju/juju/status"
	coretesting "github.com/juju/juju/testing"
	jujuversion "github.com/juju/juju/version"
)

type ProviderSuite struct {
	restoreTimeouts func()
}

var _ = gc.Suite(&ProviderSuite{})
var _ = gc.Suite(&localHTTPSServerSuite{})
var _ = gc.Suite(&noSwiftSuite{})

func (s *ProviderSuite) SetUpTest(c *gc.C) {
	s.restoreTimeouts = envtesting.PatchAttemptStrategies(openstack.ShortAttempt, openstack.StorageAttempt)
}

func (s *ProviderSuite) TearDownTest(c *gc.C) {
	s.restoreTimeouts()
}

// Register tests to run against a test Openstack instance (service doubles).
func registerLocalTests() {
	cred := &identity.Credentials{
		User:       "fred",
		Secrets:    "secret",
		Region:     "some-region",
		TenantName: "some tenant",
	}
	config := makeTestConfig(cred)
	config["agent-version"] = coretesting.FakeVersionNumber.String()
	config["authorized-keys"] = "fakekey"
	gc.Suite(&localLiveSuite{
		LiveTests: LiveTests{
			cred: cred,
			LiveTests: jujutest.LiveTests{
				TestConfig: config,
			},
		},
	})
	gc.Suite(&localServerSuite{
		cred: cred,
		Tests: jujutest.Tests{
			TestConfig: config,
		},
	})
}

// localServer is used to spin up a local Openstack service double.
type localServer struct {
	Server          *httptest.Server
	Mux             *http.ServeMux
	oldHandler      http.Handler
	Nova            *novaservice.Nova
	restoreTimeouts func()
	UseTLS          bool
}

type newOpenstackFunc func(*http.ServeMux, *identity.Credentials, identity.AuthMode) *novaservice.Nova

func (s *localServer) start(
	c *gc.C, cred *identity.Credentials, newOpenstackFunc newOpenstackFunc,
) {
	// Set up the HTTP server.
	if s.UseTLS {
		s.Server = httptest.NewTLSServer(nil)
	} else {
		s.Server = httptest.NewServer(nil)
	}
	c.Assert(s.Server, gc.NotNil)
	s.oldHandler = s.Server.Config.Handler
	s.Mux = http.NewServeMux()
	s.Server.Config.Handler = s.Mux
	cred.URL = s.Server.URL
	c.Logf("Started service at: %v", s.Server.URL)
	s.Nova = newOpenstackFunc(s.Mux, cred, identity.AuthUserPass)
	s.restoreTimeouts = envtesting.PatchAttemptStrategies(openstack.ShortAttempt, openstack.StorageAttempt)
	s.Nova.SetAvailabilityZones(
		nova.AvailabilityZone{Name: "test-unavailable"},
		nova.AvailabilityZone{
			Name: "test-available",
			State: nova.AvailabilityZoneState{
				Available: true,
			},
		},
	)
}

func (s *localServer) stop() {
	s.Mux = nil
	s.Server.Config.Handler = s.oldHandler
	s.Server.Close()
	s.restoreTimeouts()
}

// localLiveSuite runs tests from LiveTests using an Openstack service double.
type localLiveSuite struct {
	coretesting.BaseSuite
	LiveTests
	srv localServer
}

func overrideCinderProvider(c *gc.C, s *gitjujutesting.CleanupSuite) {
	s.PatchValue(openstack.NewOpenstackStorage, func(*openstack.Environ) (openstack.OpenstackStorage, error) {
		return &mockAdapter{}, nil
	})
}

func (s *localLiveSuite) SetUpSuite(c *gc.C) {
	s.BaseSuite.SetUpSuite(c)

	c.Logf("Running live tests using openstack service test double")
	s.srv.start(c, s.cred, newFullOpenstackService)

	// Set credentials to use when bootstrapping. Must be done after
	// starting server to get the auth URL.
	s.Credential = makeCredential(s.cred)
	s.CloudEndpoint = s.cred.URL
	s.CloudRegion = s.cred.Region

	s.LiveTests.SetUpSuite(c)
	openstack.UseTestImageData(openstack.ImageMetadataStorage(s.Env), s.cred)
	restoreFinishBootstrap := envtesting.DisableFinishBootstrap()
	s.AddCleanup(func(*gc.C) { restoreFinishBootstrap() })
	overrideCinderProvider(c, &s.CleanupSuite)
}

func (s *localLiveSuite) TearDownSuite(c *gc.C) {
	openstack.RemoveTestImageData(openstack.ImageMetadataStorage(s.Env))
	s.LiveTests.TearDownSuite(c)
	s.srv.stop()
	s.BaseSuite.TearDownSuite(c)
}

func (s *localLiveSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)
	s.LiveTests.SetUpTest(c)
	imagetesting.PatchOfficialDataSources(&s.CleanupSuite, "")
}

func (s *localLiveSuite) TearDownTest(c *gc.C) {
	s.LiveTests.TearDownTest(c)
	s.BaseSuite.TearDownTest(c)
}

// localServerSuite contains tests that run against an Openstack service double.
// These tests can test things that would be unreasonably slow or expensive
// to test on a live Openstack server. The service double is started and stopped for
// each test.
type localServerSuite struct {
	coretesting.BaseSuite
	jujutest.Tests
	cred                 *identity.Credentials
	srv                  localServer
	env                  environs.Environ
	toolsMetadataStorage storage.Storage
	imageMetadataStorage storage.Storage
}

func (s *localServerSuite) SetUpSuite(c *gc.C) {
	s.BaseSuite.SetUpSuite(c)
	restoreFinishBootstrap := envtesting.DisableFinishBootstrap()
	s.AddCleanup(func(*gc.C) { restoreFinishBootstrap() })
	overrideCinderProvider(c, &s.CleanupSuite)
	c.Logf("Running local tests")
}

func (s *localServerSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)
	s.srv.start(c, s.cred, newFullOpenstackService)

	// Set credentials to use when bootstrapping. Must be done after
	// starting server to get the auth URL.
	s.Credential = makeCredential(s.cred)
	s.CloudEndpoint = s.cred.URL
	s.CloudRegion = s.cred.Region

	cl := client.NewClient(s.cred, identity.AuthUserPass, nil)
	err := cl.Authenticate()
	c.Assert(err, jc.ErrorIsNil)
	containerURL, err := cl.MakeServiceURL("object-store", nil)
	c.Assert(err, jc.ErrorIsNil)
	s.TestConfig = s.TestConfig.Merge(coretesting.Attrs{
		"agent-metadata-url": containerURL + "/juju-dist-test/tools",
		"image-metadata-url": containerURL + "/juju-dist-test",
		"auth-url":           s.cred.URL,
	})
	s.PatchValue(&jujuversion.Current, coretesting.FakeVersionNumber)
	s.Tests.SetUpTest(c)
	// For testing, we create a storage instance to which is uploaded tools and image metadata.
	s.env = s.Prepare(c)
	s.toolsMetadataStorage = openstack.MetadataStorage(s.env)
	// Put some fake metadata in place so that tests that are simply
	// starting instances without any need to check if those instances
	// are running can find the metadata.
	envtesting.UploadFakeTools(c, s.toolsMetadataStorage, s.env.Config().AgentStream(), s.env.Config().AgentStream())
	s.imageMetadataStorage = openstack.ImageMetadataStorage(s.env)
	openstack.UseTestImageData(s.imageMetadataStorage, s.cred)
}

func (s *localServerSuite) TearDownTest(c *gc.C) {
	if s.imageMetadataStorage != nil {
		openstack.RemoveTestImageData(s.imageMetadataStorage)
	}
	if s.toolsMetadataStorage != nil {
		envtesting.RemoveFakeToolsMetadata(c, s.toolsMetadataStorage)
	}
	s.Tests.TearDownTest(c)
	s.srv.stop()
	s.BaseSuite.TearDownTest(c)
}

func (s *localServerSuite) openEnviron(c *gc.C, attrs coretesting.Attrs) environs.Environ {
	cfg, err := config.New(config.NoDefaults, s.TestConfig.Merge(attrs))
	c.Assert(err, jc.ErrorIsNil)
	env, err := environs.New(environs.OpenParams{
		Cloud:  s.CloudSpec(),
		Config: cfg,
	})
	c.Assert(err, jc.ErrorIsNil)
	return env
}

func (s *localServerSuite) TestBootstrap(c *gc.C) {
	// Tests uses Prepare, so destroy first.
	err := environs.Destroy(s.env.Config().Name(), s.env, s.ControllerStore)
	c.Assert(err, jc.ErrorIsNil)
	s.Tests.TestBootstrap(c)
}

func (s *localServerSuite) TestStartStop(c *gc.C) {
	// Tests uses Prepare, so destroy first.
	err := environs.Destroy(s.env.Config().Name(), s.env, s.ControllerStore)
	c.Assert(err, jc.ErrorIsNil)
	s.Tests.TestStartStop(c)
}

// If the bootstrap node is configured to require a public IP address,
// bootstrapping fails if an address cannot be allocated.
func (s *localServerSuite) TestBootstrapFailsWhenPublicIPError(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	cleanup := s.srv.Nova.RegisterControlPoint(
		"addFloatingIP",
		func(sc hook.ServiceControl, args ...interface{}) error {
			return fmt.Errorf("failed on purpose")
		},
	)
	defer cleanup()

	err := environs.Destroy(s.env.Config().Name(), s.env, s.ControllerStore)
	c.Assert(err, jc.ErrorIsNil)

	env := s.openEnviron(c, coretesting.Attrs{"use-floating-ip": true})
	err = bootstrapEnv(c, env)
	c.Assert(err, gc.ErrorMatches, "(.|\n)*cannot allocate a public IP as needed(.|\n)*")
}

func (s *localServerSuite) TestAddressesWithPublicIP(c *gc.C) {
	// Floating IP address is 10.0.0.1
	bootstrapFinished := false
	s.PatchValue(&common.FinishBootstrap, func(
		ctx environs.BootstrapContext,
		client ssh.Client,
		env environs.Environ,
		inst instance.Instance,
		instanceConfig *instancecfg.InstanceConfig,
		_ environs.BootstrapDialOpts,
	) error {
		addr, err := inst.Addresses()
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(addr, jc.SameContents, []network.Address{
			{Value: "10.0.0.1", Type: "ipv4", Scope: "public"},
			{Value: "127.0.0.1", Type: "ipv4", Scope: "local-machine"},
			{Value: "::face::000f", Type: "hostname", Scope: ""},
			{Value: "127.10.0.1", Type: "ipv4", Scope: "public"},
			{Value: "::dead:beef:f00d", Type: "ipv6", Scope: "public"},
		})
		bootstrapFinished = true
		return nil
	})

	env := s.openEnviron(c, coretesting.Attrs{"use-floating-ip": true})
	err := bootstrapEnv(c, env)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(bootstrapFinished, jc.IsTrue)
}

func (s *localServerSuite) TestAddressesWithoutPublicIP(c *gc.C) {
	bootstrapFinished := false
	s.PatchValue(&common.FinishBootstrap, func(
		ctx environs.BootstrapContext,
		client ssh.Client,
		env environs.Environ,
		inst instance.Instance,
		instanceConfig *instancecfg.InstanceConfig,
		_ environs.BootstrapDialOpts,
	) error {
		addr, err := inst.Addresses()
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(addr, jc.SameContents, []network.Address{
			{Value: "127.0.0.1", Type: "ipv4", Scope: "local-machine"},
			{Value: "::face::000f", Type: "hostname", Scope: ""},
			{Value: "127.10.0.1", Type: "ipv4", Scope: "public"},
			{Value: "::dead:beef:f00d", Type: "ipv6", Scope: "public"},
		})
		bootstrapFinished = true
		return nil
	})

	env := s.openEnviron(c, coretesting.Attrs{"use-floating-ip": false})
	err := bootstrapEnv(c, env)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(bootstrapFinished, jc.IsTrue)
}

// If the environment is configured not to require a public IP address for nodes,
// bootstrapping and starting an instance should occur without any attempt to
// allocate a public address.
func (s *localServerSuite) TestStartInstanceWithoutPublicIP(c *gc.C) {
	cleanup := s.srv.Nova.RegisterControlPoint(
		"addFloatingIP",
		func(sc hook.ServiceControl, args ...interface{}) error {
			return fmt.Errorf("add floating IP should not have been called")
		},
	)
	defer cleanup()
	cleanup = s.srv.Nova.RegisterControlPoint(
		"addServerFloatingIP",
		func(sc hook.ServiceControl, args ...interface{}) error {
			return fmt.Errorf("add server floating IP should not have been called")
		},
	)
	defer cleanup()

	err := environs.Destroy(s.env.Config().Name(), s.env, s.ControllerStore)
	c.Assert(err, jc.ErrorIsNil)

	s.TestConfig["use-floating-ip"] = false
	env := s.Prepare(c)
	err = bootstrapEnv(c, env)
	c.Assert(err, jc.ErrorIsNil)
	inst, _ := testing.AssertStartInstance(c, env, s.ControllerUUID, "100")
	err = env.StopInstances(inst.Id())
	c.Assert(err, jc.ErrorIsNil)
}

func (s *localServerSuite) TestStartInstanceHardwareCharacteristics(c *gc.C) {
	// Ensure amd64 tools are available, to ensure an amd64 image.
	amd64Version := version.Binary{
		Number: jujuversion.Current,
		Arch:   arch.AMD64,
	}
	for _, series := range series.SupportedSeries() {
		amd64Version.Series = series
		envtesting.AssertUploadFakeToolsVersions(
			c, s.toolsMetadataStorage, s.env.Config().AgentStream(), s.env.Config().AgentStream(), amd64Version)
	}

	err := environs.Destroy(s.env.Config().Name(), s.env, s.ControllerStore)
	c.Assert(err, jc.ErrorIsNil)

	env := s.Prepare(c)
	err = bootstrapEnv(c, env)
	c.Assert(err, jc.ErrorIsNil)
	_, hc := testing.AssertStartInstanceWithConstraints(c, env, s.ControllerUUID, "100", constraints.MustParse("mem=1024"))
	c.Check(*hc.Arch, gc.Equals, "amd64")
	c.Check(*hc.Mem, gc.Equals, uint64(2048))
	c.Check(*hc.CpuCores, gc.Equals, uint64(1))
	c.Assert(hc.CpuPower, gc.IsNil)
}

func (s *localServerSuite) TestStartInstanceNetwork(c *gc.C) {
	cfg, err := s.env.Config().Apply(coretesting.Attrs{
		// A label that corresponds to a nova test service network
		"network": "net",
	})
	c.Assert(err, jc.ErrorIsNil)
	err = s.env.SetConfig(cfg)
	c.Assert(err, jc.ErrorIsNil)

	inst, _ := testing.AssertStartInstance(c, s.env, s.ControllerUUID, "100")
	err = s.env.StopInstances(inst.Id())
	c.Assert(err, jc.ErrorIsNil)
}

func (s *localServerSuite) TestStartInstanceNetworkUnknownLabel(c *gc.C) {
	cfg, err := s.env.Config().Apply(coretesting.Attrs{
		// A label that has no related network in the nova test service
		"network": "no-network-with-this-label",
	})
	c.Assert(err, jc.ErrorIsNil)
	err = s.env.SetConfig(cfg)
	c.Assert(err, jc.ErrorIsNil)

	inst, _, _, err := testing.StartInstance(s.env, s.ControllerUUID, "100")
	c.Check(inst, gc.IsNil)
	c.Assert(err, gc.ErrorMatches, "No networks exist with label .*")
}

func (s *localServerSuite) TestStartInstanceNetworkUnknownId(c *gc.C) {
	cfg, err := s.env.Config().Apply(coretesting.Attrs{
		// A valid UUID but no related network in the nova test service
		"network": "f81d4fae-7dec-11d0-a765-00a0c91e6bf6",
	})
	c.Assert(err, jc.ErrorIsNil)
	err = s.env.SetConfig(cfg)
	c.Assert(err, jc.ErrorIsNil)

	inst, _, _, err := testing.StartInstance(s.env, s.ControllerUUID, "100")
	c.Check(inst, gc.IsNil)
	c.Assert(err, gc.ErrorMatches, "cannot run instance: (\\n|.)*"+
		"caused by: "+
		"request \\(.*/servers\\) returned unexpected status: "+
		"404; error info: .*itemNotFound.*")
}

func assertSecurityGroups(c *gc.C, env environs.Environ, expected []string) {
	novaClient := openstack.GetNovaClient(env)
	groups, err := novaClient.ListSecurityGroups()
	c.Assert(err, jc.ErrorIsNil)
	for _, name := range expected {
		found := false
		for _, group := range groups {
			if group.Name == name {
				found = true
				break
			}
		}
		if !found {
			c.Errorf("expected security group %q not found", name)
		}
	}
	for _, group := range groups {
		found := false
		for _, name := range expected {
			if group.Name == name {
				found = true
				break
			}
		}
		if !found {
			c.Errorf("existing security group %q is not expected", group.Name)
		}
	}
}

func assertInstanceIds(c *gc.C, env environs.Environ, expected ...instance.Id) {
	insts, err := env.AllInstances()
	c.Assert(err, jc.ErrorIsNil)
	instIds := make([]instance.Id, len(insts))
	for i, inst := range insts {
		instIds[i] = inst.Id()
	}
	c.Assert(instIds, jc.SameContents, expected)
}

func (s *localServerSuite) TestStopInstance(c *gc.C) {
	env := s.openEnviron(c, coretesting.Attrs{"firewall-mode": config.FwInstance})
	instanceName := "100"
	inst, _ := testing.AssertStartInstance(c, env, s.ControllerUUID, instanceName)
	// Openstack now has three security groups for the server, the default
	// group, one group for the entire environment, and another for the
	// new instance.
	modelUUID := env.Config().UUID()
	allSecurityGroups := []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, modelUUID),
		fmt.Sprintf("juju-%v-%v-%v", s.ControllerUUID, modelUUID, instanceName),
	}
	assertSecurityGroups(c, env, allSecurityGroups)
	err := env.StopInstances(inst.Id())
	c.Assert(err, jc.ErrorIsNil)
	// The security group for this instance is now removed.
	assertSecurityGroups(c, env, []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, modelUUID),
	})
}

// Due to bug #1300755 it can happen that the security group intended for
// an instance is also used as the common security group of another
// environment. If this is the case, the attempt to delete the instance's
// security group fails but StopInstance succeeds.
func (s *localServerSuite) TestStopInstanceSecurityGroupNotDeleted(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	// Force an error when a security group is deleted.
	cleanup := s.srv.Nova.RegisterControlPoint(
		"removeSecurityGroup",
		func(sc hook.ServiceControl, args ...interface{}) error {
			return fmt.Errorf("failed on purpose")
		},
	)
	defer cleanup()
	env := s.openEnviron(c, coretesting.Attrs{"firewall-mode": config.FwInstance})
	instanceName := "100"
	inst, _ := testing.AssertStartInstance(c, env, s.ControllerUUID, instanceName)
	modelUUID := env.Config().UUID()
	allSecurityGroups := []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, modelUUID),
		fmt.Sprintf("juju-%v-%v-%v", s.ControllerUUID, modelUUID, instanceName),
	}
	assertSecurityGroups(c, env, allSecurityGroups)
	err := env.StopInstances(inst.Id())
	c.Assert(err, jc.ErrorIsNil)
	assertSecurityGroups(c, env, allSecurityGroups)
}

func (s *localServerSuite) TestDestroyEnvironmentDeletesSecurityGroupsFWModeInstance(c *gc.C) {
	env := s.openEnviron(c, coretesting.Attrs{"firewall-mode": config.FwInstance})
	instanceName := "100"
	testing.AssertStartInstance(c, env, s.ControllerUUID, instanceName)
	modelUUID := env.Config().UUID()
	allSecurityGroups := []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, modelUUID),
		fmt.Sprintf("juju-%v-%v-%v", s.ControllerUUID, modelUUID, instanceName),
	}
	assertSecurityGroups(c, env, allSecurityGroups)
	err := env.Destroy()
	c.Check(err, jc.ErrorIsNil)
	assertSecurityGroups(c, env, []string{"default"})
}

func (s *localServerSuite) TestDestroyEnvironmentDeletesSecurityGroupsFWModeGlobal(c *gc.C) {
	env := s.openEnviron(c, coretesting.Attrs{"firewall-mode": config.FwGlobal})
	instanceName := "100"
	testing.AssertStartInstance(c, env, s.ControllerUUID, instanceName)
	modelUUID := env.Config().UUID()
	allSecurityGroups := []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, modelUUID),
		fmt.Sprintf("juju-%v-%v-global", s.ControllerUUID, modelUUID),
	}
	assertSecurityGroups(c, env, allSecurityGroups)
	err := env.Destroy()
	c.Check(err, jc.ErrorIsNil)
	assertSecurityGroups(c, env, []string{"default"})
}

func (s *localServerSuite) TestDestroyController(c *gc.C) {
	env := s.openEnviron(c, coretesting.Attrs{"uuid": utils.MustNewUUID().String()})
	controllerEnv := s.env

	controllerInstanceName := "100"
	testing.AssertStartInstance(c, controllerEnv, s.ControllerUUID, controllerInstanceName)
	hostedModelInstanceName := "200"
	testing.AssertStartInstance(c, env, s.ControllerUUID, hostedModelInstanceName)
	modelUUID := env.Config().UUID()
	allControllerSecurityGroups := []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, controllerEnv.Config().UUID()),
		fmt.Sprintf("juju-%v-%v-%v", s.ControllerUUID, controllerEnv.Config().UUID(), controllerInstanceName),
	}
	allHostedModelSecurityGroups := []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, modelUUID),
		fmt.Sprintf("juju-%v-%v-%v", s.ControllerUUID, modelUUID, hostedModelInstanceName),
	}
	assertSecurityGroups(c, controllerEnv, append(
		allControllerSecurityGroups, allHostedModelSecurityGroups...,
	))

	err := controllerEnv.DestroyController(s.ControllerUUID)
	c.Check(err, jc.ErrorIsNil)
	assertSecurityGroups(c, controllerEnv, []string{"default"})
	assertInstanceIds(c, env)
	assertInstanceIds(c, controllerEnv)
}

func (s *localServerSuite) TestDestroyHostedModel(c *gc.C) {
	env := s.openEnviron(c, coretesting.Attrs{"uuid": utils.MustNewUUID().String()})
	controllerEnv := s.env

	controllerInstanceName := "100"
	controllerInstance, _ := testing.AssertStartInstance(c, controllerEnv, s.ControllerUUID, controllerInstanceName)
	hostedModelInstanceName := "200"
	testing.AssertStartInstance(c, env, s.ControllerUUID, hostedModelInstanceName)
	modelUUID := env.Config().UUID()
	allControllerSecurityGroups := []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, controllerEnv.Config().UUID()),
		fmt.Sprintf("juju-%v-%v-%v", s.ControllerUUID, controllerEnv.Config().UUID(), controllerInstanceName),
	}
	allHostedModelSecurityGroups := []string{
		"default", fmt.Sprintf("juju-%v-%v", s.ControllerUUID, modelUUID),
		fmt.Sprintf("juju-%v-%v-%v", s.ControllerUUID, modelUUID, hostedModelInstanceName),
	}
	assertSecurityGroups(c, controllerEnv, append(
		allControllerSecurityGroups, allHostedModelSecurityGroups...,
	))

	err := env.Destroy()
	c.Check(err, jc.ErrorIsNil)
	assertSecurityGroups(c, controllerEnv, allControllerSecurityGroups)
	assertInstanceIds(c, env)
	assertInstanceIds(c, controllerEnv, controllerInstance.Id())
}

var instanceGathering = []struct {
	ids []instance.Id
	err error
}{
	{ids: []instance.Id{"id0"}},
	{ids: []instance.Id{"id0", "id0"}},
	{ids: []instance.Id{"id0", "id1"}},
	{ids: []instance.Id{"id1", "id0"}},
	{ids: []instance.Id{"id1", "id0", "id1"}},
	{
		ids: []instance.Id{""},
		err: environs.ErrNoInstances,
	},
	{
		ids: []instance.Id{"", ""},
		err: environs.ErrNoInstances,
	},
	{
		ids: []instance.Id{"", "", ""},
		err: environs.ErrNoInstances,
	},
	{
		ids: []instance.Id{"id0", ""},
		err: environs.ErrPartialInstances,
	},
	{
		ids: []instance.Id{"", "id1"},
		err: environs.ErrPartialInstances,
	},
	{
		ids: []instance.Id{"id0", "id1", ""},
		err: environs.ErrPartialInstances,
	},
	{
		ids: []instance.Id{"id0", "", "id0"},
		err: environs.ErrPartialInstances,
	},
	{
		ids: []instance.Id{"id0", "id0", ""},
		err: environs.ErrPartialInstances,
	},
	{
		ids: []instance.Id{"", "id0", "id1"},
		err: environs.ErrPartialInstances,
	},
}

func (s *localServerSuite) TestInstanceStatus(c *gc.C) {
	// goose's test service always returns ACTIVE state.
	inst, _ := testing.AssertStartInstance(c, s.env, s.ControllerUUID, "100")
	c.Assert(inst.Status().Status, gc.Equals, status.Running)
	err := s.env.StopInstances(inst.Id())
	c.Assert(err, jc.ErrorIsNil)
}

func (s *localServerSuite) TestAllInstancesFloatingIP(c *gc.C) {
	env := s.openEnviron(c, coretesting.Attrs{"use-floating-ip": true})

	inst0, _ := testing.AssertStartInstance(c, env, s.ControllerUUID, "100")
	inst1, _ := testing.AssertStartInstance(c, env, s.ControllerUUID, "101")
	defer func() {
		err := env.StopInstances(inst0.Id(), inst1.Id())
		c.Assert(err, jc.ErrorIsNil)
	}()

	insts, err := env.AllInstances()
	c.Assert(err, jc.ErrorIsNil)
	for _, inst := range insts {
		c.Assert(openstack.InstanceFloatingIP(inst).IP, gc.Equals, fmt.Sprintf("10.0.0.%v", inst.Id()))
	}
}

func (s *localServerSuite) assertInstancesGathering(c *gc.C, withFloatingIP bool) {
	env := s.openEnviron(c, coretesting.Attrs{"use-floating-ip": withFloatingIP})

	inst0, _ := testing.AssertStartInstance(c, env, s.ControllerUUID, "100")
	id0 := inst0.Id()
	inst1, _ := testing.AssertStartInstance(c, env, s.ControllerUUID, "101")
	id1 := inst1.Id()
	defer func() {
		err := env.StopInstances(inst0.Id(), inst1.Id())
		c.Assert(err, jc.ErrorIsNil)
	}()

	for i, test := range instanceGathering {
		c.Logf("test %d: find %v -> expect len %d, err: %v", i, test.ids, len(test.ids), test.err)
		ids := make([]instance.Id, len(test.ids))
		for j, id := range test.ids {
			switch id {
			case "id0":
				ids[j] = id0
			case "id1":
				ids[j] = id1
			}
		}
		insts, err := env.Instances(ids)
		c.Assert(err, gc.Equals, test.err)
		if err == environs.ErrNoInstances {
			c.Assert(insts, gc.HasLen, 0)
		} else {
			c.Assert(insts, gc.HasLen, len(test.ids))
		}
		for j, inst := range insts {
			if ids[j] != "" {
				c.Assert(inst.Id(), gc.Equals, ids[j])
				if withFloatingIP {
					c.Assert(openstack.InstanceFloatingIP(inst).IP, gc.Equals, fmt.Sprintf("10.0.0.%v", inst.Id()))
				} else {
					c.Assert(openstack.InstanceFloatingIP(inst), gc.IsNil)
				}
			} else {
				c.Assert(inst, gc.IsNil)
			}
		}
	}
}

func (s *localServerSuite) TestInstancesGathering(c *gc.C) {
	s.assertInstancesGathering(c, false)
}

func (s *localServerSuite) TestInstancesGatheringWithFloatingIP(c *gc.C) {
	s.assertInstancesGathering(c, true)
}

func (s *localServerSuite) TestInstancesBuildSpawning(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	cleanup := s.srv.Nova.RegisterControlPoint(
		"addServer",
		func(sc hook.ServiceControl, args ...interface{}) error {
			details := args[0].(*nova.ServerDetail)
			details.Status = nova.StatusBuildSpawning
			return nil
		},
	)
	defer cleanup()
	stateInst, _ := testing.AssertStartInstance(c, s.env, s.ControllerUUID, "100")
	defer func() {
		err := s.env.StopInstances(stateInst.Id())
		c.Assert(err, jc.ErrorIsNil)
	}()

	instances, err := s.env.Instances([]instance.Id{stateInst.Id()})

	c.Assert(err, jc.ErrorIsNil)
	c.Assert(instances, gc.HasLen, 1)
	c.Assert(instances[0].Status().Message, gc.Equals, nova.StatusBuildSpawning)
}

func (s *localServerSuite) TestInstancesShutoffSuspended(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	cleanup := s.srv.Nova.RegisterControlPoint(
		"addServer",
		func(sc hook.ServiceControl, args ...interface{}) error {
			details := args[0].(*nova.ServerDetail)
			switch {
			case strings.HasSuffix(details.Name, "-100"):
				details.Status = nova.StatusShutoff
			case strings.HasSuffix(details.Name, "-101"):
				details.Status = nova.StatusSuspended
			default:
				c.Fatalf("unexpected instance details: %#v", details)
			}
			return nil
		},
	)
	defer cleanup()
	stateInst1, _ := testing.AssertStartInstance(c, s.env, s.ControllerUUID, "100")
	stateInst2, _ := testing.AssertStartInstance(c, s.env, s.ControllerUUID, "101")
	defer func() {
		err := s.env.StopInstances(stateInst1.Id(), stateInst2.Id())
		c.Assert(err, jc.ErrorIsNil)
	}()

	instances, err := s.env.Instances([]instance.Id{stateInst1.Id(), stateInst2.Id()})

	c.Assert(err, jc.ErrorIsNil)
	c.Assert(instances, gc.HasLen, 2)
	c.Assert(instances[0].Status().Message, gc.Equals, nova.StatusShutoff)
	c.Assert(instances[1].Status().Message, gc.Equals, nova.StatusSuspended)
}

func (s *localServerSuite) TestInstancesErrorResponse(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	cleanup := s.srv.Nova.RegisterControlPoint(
		"server",
		func(sc hook.ServiceControl, args ...interface{}) error {
			return fmt.Errorf("strange error not instance")
		},
	)
	defer cleanup()

	instances, err := s.env.Instances([]instance.Id{"1"})
	c.Check(instances, gc.IsNil)
	c.Assert(err, gc.ErrorMatches, "(?s).*strange error not instance.*")
}

func (s *localServerSuite) TestInstancesMultiErrorResponse(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	cleanup := s.srv.Nova.RegisterControlPoint(
		"matchServers",
		func(sc hook.ServiceControl, args ...interface{}) error {
			return fmt.Errorf("strange error no instances")
		},
	)
	defer cleanup()

	instances, err := s.env.Instances([]instance.Id{"1", "2"})
	c.Check(instances, gc.IsNil)
	c.Assert(err, gc.ErrorMatches, "(?s).*strange error no instances.*")
}

// TODO (wallyworld) - this test was copied from the ec2 provider.
// It should be moved to environs.jujutests.Tests.
func (s *localServerSuite) TestBootstrapInstanceUserDataAndState(c *gc.C) {
	err := bootstrapEnv(c, s.env)
	c.Assert(err, jc.ErrorIsNil)

	// Check that ControllerInstances returns the ID of the bootstrap machine.
	ids, err := s.env.ControllerInstances(s.ControllerUUID)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(ids, gc.HasLen, 1)

	insts, err := s.env.AllInstances()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(insts, gc.HasLen, 1)
	c.Check(insts[0].Id(), gc.Equals, ids[0])

	addresses, err := insts[0].Addresses()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(addresses, gc.Not(gc.HasLen), 0)

	// TODO(wallyworld) - 2013-03-01 bug=1137005
	// The nova test double needs to be updated to support retrieving instance userData.
	// Until then, we can't check the cloud init script was generated correctly.
	// When we can, we should also check cloudinit for a non-manager node (as in the
	// ec2 tests).
}

func (s *localServerSuite) assertGetImageMetadataSources(c *gc.C, stream, officialSourcePath string) {
	// Create a config that matches s.TestConfig but with the specified stream.
	attrs := coretesting.Attrs{}
	if stream != "" {
		attrs = coretesting.Attrs{"image-stream": stream}
	}
	env := s.openEnviron(c, attrs)

	sources, err := environs.ImageMetadataSources(env)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(sources, gc.HasLen, 4)
	var urls = make([]string, len(sources))
	for i, source := range sources {
		url, err := source.URL("")
		c.Assert(err, jc.ErrorIsNil)
		urls[i] = url
	}
	// The image-metadata-url ends with "/juju-dist-test/".
	c.Check(strings.HasSuffix(urls[0], "/juju-dist-test/"), jc.IsTrue)
	// The product-streams URL ends with "/imagemetadata".
	c.Check(strings.HasSuffix(urls[1], "/imagemetadata/"), jc.IsTrue)
	c.Assert(urls[2], gc.Equals, fmt.Sprintf("https://streams.canonical.com/juju/images/%s/", officialSourcePath))
	c.Assert(urls[3], gc.Equals, fmt.Sprintf("http://cloud-images.ubuntu.com/%s/", officialSourcePath))
}

func (s *localServerSuite) TestGetImageMetadataSources(c *gc.C) {
	s.assertGetImageMetadataSources(c, "", "releases")
	s.assertGetImageMetadataSources(c, "released", "releases")
	s.assertGetImageMetadataSources(c, "daily", "daily")
}

func (s *localServerSuite) TestGetImageMetadataSourcesNoProductStreams(c *gc.C) {
	s.PatchValue(openstack.MakeServiceURL, func(client.AuthenticatingClient, string, []string) (string, error) {
		return "", errors.New("cannae do it captain")
	})
	env := s.Open(c, s.env.Config())
	sources, err := environs.ImageMetadataSources(env)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(sources, gc.HasLen, 3)

	// Check that data sources are in the right order
	c.Check(sources[0].Description(), gc.Equals, "image-metadata-url")
	c.Check(sources[1].Description(), gc.Equals, "default cloud images")
	c.Check(sources[2].Description(), gc.Equals, "default ubuntu cloud images")
}

func (s *localServerSuite) TestGetToolsMetadataSources(c *gc.C) {
	s.PatchValue(&tools.DefaultBaseURL, "")

	env := s.Open(c, s.env.Config())
	sources, err := tools.GetMetadataSources(env)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(sources, gc.HasLen, 2)
	var urls = make([]string, len(sources))
	for i, source := range sources {
		url, err := source.URL("")
		c.Assert(err, jc.ErrorIsNil)
		urls[i] = url
	}
	// The agent-metadata-url ends with "/juju-dist-test/tools/".
	c.Check(strings.HasSuffix(urls[0], "/juju-dist-test/tools/"), jc.IsTrue)
	// Check that the URL from keystone parses.
	_, err = url.Parse(urls[1])
	c.Assert(err, jc.ErrorIsNil)
}

func (s *localServerSuite) TestSupportsNetworking(c *gc.C) {
	env := s.Open(c, s.env.Config())
	_, ok := environs.SupportsNetworking(env)
	c.Assert(ok, jc.IsFalse)
}

func (s *localServerSuite) TestFindImageBadDefaultImage(c *gc.C) {
	imagetesting.PatchOfficialDataSources(&s.CleanupSuite, "")
	env := s.Open(c, s.env.Config())

	// An error occurs if no suitable image is found.
	_, err := openstack.FindInstanceSpec(env, "saucy", "amd64", "mem=1G", nil)
	c.Assert(err, gc.ErrorMatches, `no "saucy" images in some-region with arches \[amd64\]`)
}

func (s *localServerSuite) TestConstraintsValidator(c *gc.C) {
	env := s.Open(c, s.env.Config())
	validator, err := env.ConstraintsValidator()
	c.Assert(err, jc.ErrorIsNil)
	cons := constraints.MustParse("arch=amd64 cpu-power=10 virt-type=lxd")
	unsupported, err := validator.Validate(cons)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(unsupported, jc.SameContents, []string{"cpu-power"})
}

func (s *localServerSuite) TestConstraintsValidatorVocab(c *gc.C) {
	env := s.Open(c, s.env.Config())
	validator, err := env.ConstraintsValidator()
	c.Assert(err, jc.ErrorIsNil)

	cons := constraints.MustParse("instance-type=foo")
	_, err = validator.Validate(cons)
	c.Assert(err, gc.ErrorMatches, "invalid constraint value: instance-type=foo\nvalid values are:.*")

	cons = constraints.MustParse("virt-type=foo")
	_, err = validator.Validate(cons)
	c.Assert(err, gc.ErrorMatches, regexp.QuoteMeta("invalid constraint value: virt-type=foo\nvalid values are: [kvm lxd]"))
}

func (s *localServerSuite) TestConstraintsMerge(c *gc.C) {
	env := s.Open(c, s.env.Config())
	validator, err := env.ConstraintsValidator()
	c.Assert(err, jc.ErrorIsNil)
	consA := constraints.MustParse("arch=amd64 mem=1G root-disk=10G")
	consB := constraints.MustParse("instance-type=m1.small")
	cons, err := validator.Merge(consA, consB)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(cons, gc.DeepEquals, constraints.MustParse("arch=amd64 instance-type=m1.small"))
}

func (s *localServerSuite) TestFindImageInstanceConstraint(c *gc.C) {
	env := s.Open(c, s.env.Config())
	imageMetadata := []*imagemetadata.ImageMetadata{{
		Id:   "image-id",
		Arch: "amd64",
	}}

	spec, err := openstack.FindInstanceSpec(
		env, series.LatestLts(), "amd64", "instance-type=m1.tiny",
		imageMetadata,
	)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(spec.InstanceType.Name, gc.Equals, "m1.tiny")
}

func (s *localServerSuite) TestFindInstanceImageConstraintHypervisor(c *gc.C) {
	testVirtType := "qemu"
	env := s.Open(c, s.env.Config())
	imageMetadata := []*imagemetadata.ImageMetadata{{
		Id:       "image-id",
		Arch:     "amd64",
		VirtType: testVirtType,
	}}

	spec, err := openstack.FindInstanceSpec(
		env, series.LatestLts(), "amd64", "virt-type="+testVirtType,
		imageMetadata,
	)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(spec.InstanceType.VirtType, gc.NotNil)
	c.Assert(*spec.InstanceType.VirtType, gc.Equals, testVirtType)
	c.Assert(spec.InstanceType.Name, gc.Equals, "m1.small")
}

func (s *localServerSuite) TestFindInstanceImageWithHypervisorNoConstraint(c *gc.C) {
	testVirtType := "qemu"
	env := s.Open(c, s.env.Config())
	imageMetadata := []*imagemetadata.ImageMetadata{{
		Id:       "image-id",
		Arch:     "amd64",
		VirtType: testVirtType,
	}}

	spec, err := openstack.FindInstanceSpec(
		env, series.LatestLts(), "amd64", "",
		imageMetadata,
	)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(spec.InstanceType.VirtType, gc.NotNil)
	c.Assert(*spec.InstanceType.VirtType, gc.Equals, testVirtType)
	c.Assert(spec.InstanceType.Name, gc.Equals, "m1.small")
}

func (s *localServerSuite) TestFindInstanceNoConstraint(c *gc.C) {
	env := s.Open(c, s.env.Config())
	imageMetadata := []*imagemetadata.ImageMetadata{{
		Id:   "image-id",
		Arch: "amd64",
	}}

	spec, err := openstack.FindInstanceSpec(
		env, series.LatestLts(), "amd64", "",
		imageMetadata,
	)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(spec.InstanceType.VirtType, gc.IsNil)
	c.Assert(spec.InstanceType.Name, gc.Equals, "m1.small")
}

func (s *localServerSuite) TestFindImageInvalidInstanceConstraint(c *gc.C) {
	env := s.Open(c, s.env.Config())
	imageMetadata := []*imagemetadata.ImageMetadata{{
		Id:   "image-id",
		Arch: "amd64",
	}}
	_, err := openstack.FindInstanceSpec(
		env, series.LatestLts(), "amd64", "instance-type=m1.large",
		imageMetadata,
	)
	c.Assert(err, gc.ErrorMatches, `no instance types in some-region matching constraints "instance-type=m1.large"`)
}

func (s *localServerSuite) TestPrecheckInstanceValidInstanceType(c *gc.C) {
	env := s.Open(c, s.env.Config())
	cons := constraints.MustParse("instance-type=m1.small")
	placement := ""
	err := env.PrecheckInstance(series.LatestLts(), cons, placement)
	c.Assert(err, jc.ErrorIsNil)
}

func (s *localServerSuite) TestPrecheckInstanceInvalidInstanceType(c *gc.C) {
	env := s.Open(c, s.env.Config())
	cons := constraints.MustParse("instance-type=m1.large")
	placement := ""
	err := env.PrecheckInstance(series.LatestLts(), cons, placement)
	c.Assert(err, gc.ErrorMatches, `invalid Openstack flavour "m1.large" specified`)
}

func (t *localServerSuite) TestPrecheckInstanceAvailZone(c *gc.C) {
	placement := "zone=test-available"
	err := t.env.PrecheckInstance(series.LatestLts(), constraints.Value{}, placement)
	c.Assert(err, jc.ErrorIsNil)
}

func (t *localServerSuite) TestPrecheckInstanceAvailZoneUnavailable(c *gc.C) {
	placement := "zone=test-unavailable"
	err := t.env.PrecheckInstance(series.LatestLts(), constraints.Value{}, placement)
	c.Assert(err, jc.ErrorIsNil)
}

func (t *localServerSuite) TestPrecheckInstanceAvailZoneUnknown(c *gc.C) {
	placement := "zone=test-unknown"
	err := t.env.PrecheckInstance(series.LatestLts(), constraints.Value{}, placement)
	c.Assert(err, gc.ErrorMatches, `invalid availability zone "test-unknown"`)
}

func (t *localServerSuite) TestPrecheckInstanceAvailZonesUnsupported(c *gc.C) {
	t.srv.Nova.SetAvailabilityZones() // no availability zone support
	placement := "zone=test-unknown"
	err := t.env.PrecheckInstance(series.LatestLts(), constraints.Value{}, placement)
	c.Assert(err, jc.Satisfies, jujuerrors.IsNotImplemented)
}

func (s *localServerSuite) TestValidateImageMetadata(c *gc.C) {
	env := s.Open(c, s.env.Config())
	params, err := env.(simplestreams.MetadataValidator).MetadataLookupParams("some-region")
	c.Assert(err, jc.ErrorIsNil)
	params.Sources, err = environs.ImageMetadataSources(env)
	c.Assert(err, jc.ErrorIsNil)
	params.Series = "raring"
	image_ids, _, err := imagemetadata.ValidateImageMetadata(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(image_ids, jc.SameContents, []string{"id-y"})
}

func (s *localServerSuite) TestImageMetadataSourceOrder(c *gc.C) {
	src := func(env environs.Environ) (simplestreams.DataSource, error) {
		return simplestreams.NewURLDataSource("my datasource", "bar", false, simplestreams.CUSTOM_CLOUD_DATA, false), nil
	}
	environs.RegisterUserImageDataSourceFunc("my func", src)
	env := s.Open(c, s.env.Config())
	sources, err := environs.ImageMetadataSources(env)
	c.Assert(err, jc.ErrorIsNil)
	var sourceIds []string
	for _, s := range sources {
		sourceIds = append(sourceIds, s.Description())
	}
	c.Assert(sourceIds, jc.DeepEquals, []string{
		"image-metadata-url", "my datasource", "keystone catalog", "default cloud images", "default ubuntu cloud images"})
}

// TestEnsureGroup checks that when creating a duplicate security group, the existing group is
// returned and the existing rules have been left as is.
func (s *localServerSuite) TestEnsureGroup(c *gc.C) {
	rule := []nova.RuleInfo{
		{
			IPProtocol: "tcp",
			FromPort:   22,
			ToPort:     22,
		},
	}

	assertRule := func(group nova.SecurityGroup) {
		c.Check(len(group.Rules), gc.Equals, 1)
		c.Check(*group.Rules[0].IPProtocol, gc.Equals, "tcp")
		c.Check(*group.Rules[0].FromPort, gc.Equals, 22)
		c.Check(*group.Rules[0].ToPort, gc.Equals, 22)
	}

	group, err := openstack.EnsureGroup(s.env, "test group", rule)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(group.Name, gc.Equals, "test group")
	assertRule(group)
	id := group.Id
	// Do it again and check that the existing group is returned.
	anotherRule := []nova.RuleInfo{
		{
			IPProtocol: "tcp",
			FromPort:   1,
			ToPort:     65535,
		},
	}
	group, err = openstack.EnsureGroup(s.env, "test group", anotherRule)
	c.Assert(err, jc.ErrorIsNil)
	c.Check(group.Id, gc.Equals, id)
	c.Assert(group.Name, gc.Equals, "test group")
	assertRule(group)
}

// localHTTPSServerSuite contains tests that run against an Openstack service
// double connected on an HTTPS port with a self-signed certificate. This
// service is set up and torn down for every test.  This should only test
// things that depend on the HTTPS connection, all other functional tests on a
// local connection should be in localServerSuite
type localHTTPSServerSuite struct {
	coretesting.BaseSuite
	attrs map[string]interface{}
	cred  *identity.Credentials
	srv   localServer
	env   environs.Environ
}

func (s *localHTTPSServerSuite) SetUpSuite(c *gc.C) {
	s.BaseSuite.SetUpSuite(c)
	overrideCinderProvider(c, &s.CleanupSuite)
}

func (s *localHTTPSServerSuite) createConfigAttrs(c *gc.C) map[string]interface{} {
	attrs := makeTestConfig(s.cred)
	attrs["agent-version"] = coretesting.FakeVersionNumber.String()
	attrs["authorized-keys"] = "fakekey"
	// In order to set up and tear down the environment properly, we must
	// disable hostname verification
	attrs["ssl-hostname-verification"] = false
	attrs["auth-url"] = s.cred.URL
	// Now connect and set up test-local tools and image-metadata URLs
	cl := client.NewNonValidatingClient(s.cred, identity.AuthUserPass, nil)
	err := cl.Authenticate()
	c.Assert(err, jc.ErrorIsNil)
	containerURL, err := cl.MakeServiceURL("object-store", nil)
	c.Assert(err, jc.ErrorIsNil)
	c.Check(containerURL[:8], gc.Equals, "https://")
	attrs["agent-metadata-url"] = containerURL + "/juju-dist-test/tools"
	c.Logf("Set agent-metadata-url=%q", attrs["agent-metadata-url"])
	attrs["image-metadata-url"] = containerURL + "/juju-dist-test"
	c.Logf("Set image-metadata-url=%q", attrs["image-metadata-url"])
	return attrs
}

func (s *localHTTPSServerSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)
	s.PatchValue(&jujuversion.Current, coretesting.FakeVersionNumber)
	s.srv.UseTLS = true
	cred := &identity.Credentials{
		User:       "fred",
		Secrets:    "secret",
		Region:     "some-region",
		TenantName: "some tenant",
	}
	// Note: start() will change cred.URL to point to s.srv.Server.URL
	s.srv.start(c, cred, newFullOpenstackService)
	s.cred = cred
	attrs := s.createConfigAttrs(c)
	c.Assert(attrs["auth-url"].(string)[:8], gc.Equals, "https://")
	var err error
	s.env, err = bootstrap.Prepare(
		envtesting.BootstrapContext(c),
		jujuclienttesting.NewMemStore(),
		prepareParams(attrs, s.cred),
	)
	c.Assert(err, jc.ErrorIsNil)
	s.attrs = s.env.Config().AllAttrs()
}

func (s *localHTTPSServerSuite) TearDownTest(c *gc.C) {
	if s.env != nil {
		err := s.env.Destroy()
		c.Check(err, jc.ErrorIsNil)
		s.env = nil
	}
	s.srv.stop()
	s.BaseSuite.TearDownTest(c)
}

func (s *localHTTPSServerSuite) TestMustDisableSSLVerify(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	// If you don't have ssl-hostname-verification set to false, then we
	// fail to connect to the environment. Copy the attrs used by SetUp and
	// force hostname verification.
	newattrs := make(map[string]interface{}, len(s.attrs))
	for k, v := range s.attrs {
		newattrs[k] = v
	}
	newattrs["ssl-hostname-verification"] = true
	cfg, err := config.New(config.NoDefaults, newattrs)
	c.Assert(err, jc.ErrorIsNil)
	env, err := environs.New(environs.OpenParams{
		Cloud:  makeCloudSpec(s.cred),
		Config: cfg,
	})
	c.Assert(err, jc.ErrorIsNil)
	_, err = env.AllInstances()
	c.Assert(err, gc.ErrorMatches, "(.|\n)*x509: certificate signed by unknown authority")
}

func (s *localHTTPSServerSuite) TestCanBootstrap(c *gc.C) {
	restoreFinishBootstrap := envtesting.DisableFinishBootstrap()
	defer restoreFinishBootstrap()

	// For testing, we create a storage instance to which is uploaded tools and image metadata.
	metadataStorage := openstack.MetadataStorage(s.env)
	url, err := metadataStorage.URL("")
	c.Assert(err, jc.ErrorIsNil)
	c.Logf("Generating fake tools for: %v", url)
	envtesting.UploadFakeTools(c, metadataStorage, s.env.Config().AgentStream(), s.env.Config().AgentStream())
	defer envtesting.RemoveFakeTools(c, metadataStorage, s.env.Config().AgentStream())
	openstack.UseTestImageData(metadataStorage, s.cred)
	defer openstack.RemoveTestImageData(metadataStorage)

	err = bootstrapEnv(c, s.env)
	c.Assert(err, jc.ErrorIsNil)
}

func (s *localHTTPSServerSuite) TestFetchFromImageMetadataSources(c *gc.C) {
	// Setup a custom URL for image metadata
	customStorage := openstack.CreateCustomStorage(s.env, "custom-metadata")
	customURL, err := customStorage.URL("")
	c.Assert(err, jc.ErrorIsNil)
	c.Check(customURL[:8], gc.Equals, "https://")

	config, err := s.env.Config().Apply(
		map[string]interface{}{"image-metadata-url": customURL},
	)
	c.Assert(err, jc.ErrorIsNil)
	err = s.env.SetConfig(config)
	c.Assert(err, jc.ErrorIsNil)
	sources, err := environs.ImageMetadataSources(s.env)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(sources, gc.HasLen, 4)

	// Make sure there is something to download from each location
	metadata := "metadata-content"
	metadataStorage := openstack.ImageMetadataStorage(s.env)
	err = metadataStorage.Put(metadata, bytes.NewBufferString(metadata), int64(len(metadata)))
	c.Assert(err, jc.ErrorIsNil)

	custom := "custom-content"
	err = customStorage.Put(custom, bytes.NewBufferString(custom), int64(len(custom)))
	c.Assert(err, jc.ErrorIsNil)

	// Produce map of data sources keyed on description
	mappedSources := make(map[string]simplestreams.DataSource, len(sources))
	for i, s := range sources {
		c.Logf("datasource %d: %+v", i, s)
		mappedSources[s.Description()] = s
	}

	// Read from the Config entry's image-metadata-url
	contentReader, url, err := mappedSources["image-metadata-url"].Fetch(custom)
	c.Assert(err, jc.ErrorIsNil)
	defer contentReader.Close()
	content, err := ioutil.ReadAll(contentReader)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(string(content), gc.Equals, custom)
	c.Check(url[:8], gc.Equals, "https://")

	// Check the entry we got from keystone
	contentReader, url, err = mappedSources["keystone catalog"].Fetch(metadata)
	c.Assert(err, jc.ErrorIsNil)
	defer contentReader.Close()
	content, err = ioutil.ReadAll(contentReader)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(string(content), gc.Equals, metadata)
	c.Check(url[:8], gc.Equals, "https://")
	// Verify that we are pointing at exactly where metadataStorage thinks we are
	metaURL, err := metadataStorage.URL(metadata)
	c.Assert(err, jc.ErrorIsNil)
	c.Check(url, gc.Equals, metaURL)

}

func (s *localHTTPSServerSuite) TestFetchFromToolsMetadataSources(c *gc.C) {
	// Setup a custom URL for image metadata
	customStorage := openstack.CreateCustomStorage(s.env, "custom-tools-metadata")
	customURL, err := customStorage.URL("")
	c.Assert(err, jc.ErrorIsNil)
	c.Check(customURL[:8], gc.Equals, "https://")

	config, err := s.env.Config().Apply(
		map[string]interface{}{"agent-metadata-url": customURL},
	)
	c.Assert(err, jc.ErrorIsNil)
	err = s.env.SetConfig(config)
	c.Assert(err, jc.ErrorIsNil)
	sources, err := tools.GetMetadataSources(s.env)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(sources, gc.HasLen, 3)

	// Make sure there is something to download from each location

	keystone := "keystone-tools-content"
	// The keystone entry just points at the root of the Swift storage, and
	// we have to create a container to upload any data. So we just point
	// into a subdirectory for the data we are downloading
	keystoneContainer := "tools-test"
	keystoneStorage := openstack.CreateCustomStorage(s.env, "tools-test")
	err = keystoneStorage.Put(keystone, bytes.NewBufferString(keystone), int64(len(keystone)))
	c.Assert(err, jc.ErrorIsNil)

	custom := "custom-tools-content"
	err = customStorage.Put(custom, bytes.NewBufferString(custom), int64(len(custom)))
	c.Assert(err, jc.ErrorIsNil)

	// Read from the Config entry's agent-metadata-url
	contentReader, url, err := sources[0].Fetch(custom)
	c.Assert(err, jc.ErrorIsNil)
	defer contentReader.Close()
	content, err := ioutil.ReadAll(contentReader)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(string(content), gc.Equals, custom)
	c.Check(url[:8], gc.Equals, "https://")

	// Check the entry we got from keystone
	// Now fetch the data, and verify the contents.
	contentReader, url, err = sources[1].Fetch(keystoneContainer + "/" + keystone)
	c.Assert(err, jc.ErrorIsNil)
	defer contentReader.Close()
	content, err = ioutil.ReadAll(contentReader)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(string(content), gc.Equals, keystone)
	c.Check(url[:8], gc.Equals, "https://")
	keystoneURL, err := keystoneStorage.URL(keystone)
	c.Assert(err, jc.ErrorIsNil)
	c.Check(url, gc.Equals, keystoneURL)

	// We *don't* test Fetch for sources[3] because it points to
	// streams.canonical.com
}

func (s *localServerSuite) TestRemoveBlankContainer(c *gc.C) {
	storage := openstack.BlankContainerStorage()
	err := storage.Remove("some-file")
	c.Assert(err, gc.ErrorMatches, `cannot remove "some-file": swift container name is empty`)
}

func (s *localServerSuite) TestAllInstancesIgnoresOtherMachines(c *gc.C) {
	err := bootstrapEnv(c, s.env)
	c.Assert(err, jc.ErrorIsNil)

	// Check that we see 1 instance in the environment
	insts, err := s.env.AllInstances()
	c.Assert(err, jc.ErrorIsNil)
	c.Check(insts, gc.HasLen, 1)

	// Now start a machine 'manually' in the same account, with a similar
	// but not matching name, and ensure it isn't seen by AllInstances
	// See bug #1257481, for how similar names were causing them to get
	// listed (and thus destroyed) at the wrong time
	existingModelName := s.TestConfig["name"]
	newMachineName := fmt.Sprintf("juju-%s-2-machine-0", existingModelName)

	// We grab the Nova client directly from the env, just to save time
	// looking all the stuff up
	novaClient := openstack.GetNovaClient(s.env)
	entity, err := novaClient.RunServer(nova.RunServerOpts{
		Name:     newMachineName,
		FlavorId: "1", // test service has 1,2,3 for flavor ids
		ImageId:  "1", // UseTestImageData sets up images 1 and 2
	})
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(entity, gc.NotNil)

	// List all servers with no filter, we should see both instances
	servers, err := novaClient.ListServersDetail(nova.NewFilter())
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(servers, gc.HasLen, 2)

	insts, err = s.env.AllInstances()
	c.Assert(err, jc.ErrorIsNil)
	c.Check(insts, gc.HasLen, 1)
}

func (s *localServerSuite) TestResolveNetworkUUID(c *gc.C) {
	var sampleUUID = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	networkId, err := openstack.ResolveNetwork(s.env, sampleUUID)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(networkId, gc.Equals, sampleUUID)
}

func (s *localServerSuite) TestResolveNetworkLabel(c *gc.C) {
	// For now this test has to cheat and use knowledge of goose internals
	var networkLabel = "net"
	var expectNetworkId = "1"
	networkId, err := openstack.ResolveNetwork(s.env, networkLabel)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(networkId, gc.Equals, expectNetworkId)
}

func (s *localServerSuite) TestResolveNetworkNotPresent(c *gc.C) {
	var notPresentNetwork = "no-network-with-this-label"
	networkId, err := openstack.ResolveNetwork(s.env, notPresentNetwork)
	c.Check(networkId, gc.Equals, "")
	c.Assert(err, gc.ErrorMatches, `No networks exist with label "no-network-with-this-label"`)
}

// TODO(gz): TestResolveNetworkMultipleMatching when can inject new networks

func (t *localServerSuite) TestStartInstanceAvailZone(c *gc.C) {
	inst, err := t.testStartInstanceAvailZone(c, "test-available")
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(openstack.InstanceServerDetail(inst).AvailabilityZone, gc.Equals, "test-available")
}

func (t *localServerSuite) TestStartInstanceAvailZoneUnavailable(c *gc.C) {
	_, err := t.testStartInstanceAvailZone(c, "test-unavailable")
	c.Assert(err, gc.ErrorMatches, `availability zone "test-unavailable" is unavailable`)
}

func (t *localServerSuite) TestStartInstanceAvailZoneUnknown(c *gc.C) {
	_, err := t.testStartInstanceAvailZone(c, "test-unknown")
	c.Assert(err, gc.ErrorMatches, `invalid availability zone "test-unknown"`)
}

func (t *localServerSuite) testStartInstanceAvailZone(c *gc.C, zone string) (instance.Instance, error) {
	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	params := environs.StartInstanceParams{
		ControllerUUID: t.ControllerUUID,
		Placement:      "zone=" + zone,
	}
	result, err := testing.StartInstanceWithParams(t.env, "1", params)
	if err != nil {
		return nil, err
	}
	return result.Instance, nil
}

func (t *localServerSuite) TestGetAvailabilityZones(c *gc.C) {
	var resultZones []nova.AvailabilityZone
	var resultErr error
	t.PatchValue(openstack.NovaListAvailabilityZones, func(c *nova.Client) ([]nova.AvailabilityZone, error) {
		return append([]nova.AvailabilityZone{}, resultZones...), resultErr
	})
	env := t.env.(common.ZonedEnviron)

	resultErr = fmt.Errorf("failed to get availability zones")
	zones, err := env.AvailabilityZones()
	c.Assert(err, gc.Equals, resultErr)
	c.Assert(zones, gc.IsNil)

	resultErr = nil
	resultZones = make([]nova.AvailabilityZone, 1)
	resultZones[0].Name = "whatever"
	zones, err = env.AvailabilityZones()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(zones, gc.HasLen, 1)
	c.Assert(zones[0].Name(), gc.Equals, "whatever")

	// A successful result is cached, currently for the lifetime
	// of the Environ. This will change if/when we have long-lived
	// Environs to cut down repeated IaaS requests.
	resultErr = fmt.Errorf("failed to get availability zones")
	resultZones[0].Name = "andever"
	zones, err = env.AvailabilityZones()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(zones, gc.HasLen, 1)
	c.Assert(zones[0].Name(), gc.Equals, "whatever")
}

func (t *localServerSuite) TestGetAvailabilityZonesCommon(c *gc.C) {
	var resultZones []nova.AvailabilityZone
	t.PatchValue(openstack.NovaListAvailabilityZones, func(c *nova.Client) ([]nova.AvailabilityZone, error) {
		return append([]nova.AvailabilityZone{}, resultZones...), nil
	})
	env := t.env.(common.ZonedEnviron)
	resultZones = make([]nova.AvailabilityZone, 2)
	resultZones[0].Name = "az1"
	resultZones[1].Name = "az2"
	resultZones[0].State.Available = true
	resultZones[1].State.Available = false
	zones, err := env.AvailabilityZones()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(zones, gc.HasLen, 2)
	c.Assert(zones[0].Name(), gc.Equals, resultZones[0].Name)
	c.Assert(zones[1].Name(), gc.Equals, resultZones[1].Name)
	c.Assert(zones[0].Available(), jc.IsTrue)
	c.Assert(zones[1].Available(), jc.IsFalse)
}

type mockAvailabilityZoneAllocations struct {
	group  []instance.Id // input param
	result []common.AvailabilityZoneInstances
	err    error
}

func (t *mockAvailabilityZoneAllocations) AvailabilityZoneAllocations(
	e common.ZonedEnviron, group []instance.Id,
) ([]common.AvailabilityZoneInstances, error) {
	t.group = group
	return t.result, t.err
}

func (t *localServerSuite) TestStartInstanceDistributionParams(c *gc.C) {
	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	var mock mockAvailabilityZoneAllocations
	t.PatchValue(openstack.AvailabilityZoneAllocations, mock.AvailabilityZoneAllocations)

	// no distribution group specified
	testing.AssertStartInstance(c, t.env, t.ControllerUUID, "1")
	c.Assert(mock.group, gc.HasLen, 0)

	// distribution group specified: ensure it's passed through to AvailabilityZone.
	expectedInstances := []instance.Id{"i-0", "i-1"}
	params := environs.StartInstanceParams{
		ControllerUUID: t.ControllerUUID,
		DistributionGroup: func() ([]instance.Id, error) {
			return expectedInstances, nil
		},
	}
	_, err = testing.StartInstanceWithParams(t.env, "1", params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(mock.group, gc.DeepEquals, expectedInstances)
}

func (t *localServerSuite) TestStartInstanceDistributionErrors(c *gc.C) {
	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	mock := mockAvailabilityZoneAllocations{
		err: fmt.Errorf("AvailabilityZoneAllocations failed"),
	}
	t.PatchValue(openstack.AvailabilityZoneAllocations, mock.AvailabilityZoneAllocations)
	_, _, _, err = testing.StartInstance(t.env, t.ControllerUUID, "1")
	c.Assert(jujuerrors.Cause(err), gc.Equals, mock.err)

	mock.err = nil
	dgErr := fmt.Errorf("DistributionGroup failed")
	params := environs.StartInstanceParams{
		ControllerUUID: t.ControllerUUID,
		DistributionGroup: func() ([]instance.Id, error) {
			return nil, dgErr
		},
	}
	_, err = testing.StartInstanceWithParams(t.env, "1", params)
	c.Assert(jujuerrors.Cause(err), gc.Equals, dgErr)
}

func (t *localServerSuite) TestStartInstanceDistribution(c *gc.C) {
	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	// test-available is the only available AZ, so AvailabilityZoneAllocations
	// is guaranteed to return that.
	inst, _ := testing.AssertStartInstance(c, t.env, t.ControllerUUID, "1")
	c.Assert(openstack.InstanceServerDetail(inst).AvailabilityZone, gc.Equals, "test-available")
}

func (t *localServerSuite) TestStartInstancePicksValidZoneForHost(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	t.srv.Nova.SetAvailabilityZones(
		// bootstrap node will be on az1.
		nova.AvailabilityZone{
			Name: "az1",
			State: nova.AvailabilityZoneState{
				Available: true,
			},
		},
		// az2 will be made to return an error.
		nova.AvailabilityZone{
			Name: "az2",
			State: nova.AvailabilityZoneState{
				Available: true,
			},
		},
		// az3 will be valid to host an instance.
		nova.AvailabilityZone{
			Name: "az3",
			State: nova.AvailabilityZoneState{
				Available: true,
			},
		},
	)

	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	cleanup := t.srv.Nova.RegisterControlPoint(
		"addServer",
		func(sc hook.ServiceControl, args ...interface{}) error {
			serverDetail := args[0].(*nova.ServerDetail)
			if serverDetail.AvailabilityZone == "az2" {
				return fmt.Errorf("No valid host was found")
			}
			return nil
		},
	)
	defer cleanup()
	inst, _ := testing.AssertStartInstance(c, t.env, t.ControllerUUID, "1")
	c.Assert(openstack.InstanceServerDetail(inst).AvailabilityZone, gc.Equals, "az3")
}

func (t *localServerSuite) TestStartInstanceWithUnknownAZError(c *gc.C) {
	coretesting.SkipIfPPC64EL(c, "lp:1425242")

	t.srv.Nova.SetAvailabilityZones(
		// bootstrap node will be on az1.
		nova.AvailabilityZone{
			Name: "az1",
			State: nova.AvailabilityZoneState{
				Available: true,
			},
		},
		// az2 will be made to return an unknown error.
		nova.AvailabilityZone{
			Name: "az2",
			State: nova.AvailabilityZoneState{
				Available: true,
			},
		},
	)

	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	cleanup := t.srv.Nova.RegisterControlPoint(
		"addServer",
		func(sc hook.ServiceControl, args ...interface{}) error {
			serverDetail := args[0].(*nova.ServerDetail)
			if serverDetail.AvailabilityZone == "az2" {
				return fmt.Errorf("Some unknown error")
			}
			return nil
		},
	)
	defer cleanup()
	_, _, _, err = testing.StartInstance(t.env, t.ControllerUUID, "1")
	c.Assert(err, gc.ErrorMatches, "(?s).*Some unknown error.*")
}

func (t *localServerSuite) TestStartInstanceDistributionAZNotImplemented(c *gc.C) {
	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	mock := mockAvailabilityZoneAllocations{
		err: jujuerrors.NotImplementedf("availability zones"),
	}
	t.PatchValue(openstack.AvailabilityZoneAllocations, mock.AvailabilityZoneAllocations)

	// Instance will be created without an availability zone specified.
	inst, _ := testing.AssertStartInstance(c, t.env, t.ControllerUUID, "1")
	c.Assert(openstack.InstanceServerDetail(inst).AvailabilityZone, gc.Equals, "")
}

func (t *localServerSuite) TestInstanceTags(c *gc.C) {
	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	instances, err := t.env.AllInstances()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(instances, gc.HasLen, 1)

	c.Assert(
		openstack.InstanceServerDetail(instances[0]).Metadata,
		jc.DeepEquals,
		map[string]string{
			"juju-model-uuid":      coretesting.ModelTag.Id(),
			"juju-controller-uuid": coretesting.ControllerTag.Id(),
			"juju-is-controller":   "true",
		},
	)
}

func (t *localServerSuite) TestTagInstance(c *gc.C) {
	err := bootstrapEnv(c, t.env)
	c.Assert(err, jc.ErrorIsNil)

	assertMetadata := func(extraKey, extraValue string) {
		// Refresh instance
		instances, err := t.env.AllInstances()
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(instances, gc.HasLen, 1)
		c.Assert(
			openstack.InstanceServerDetail(instances[0]).Metadata,
			jc.DeepEquals,
			map[string]string{
				"juju-model-uuid":      coretesting.ModelTag.Id(),
				"juju-controller-uuid": coretesting.ControllerTag.Id(),
				"juju-is-controller":   "true",
				extraKey:               extraValue,
			},
		)
	}

	instances, err := t.env.AllInstances()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(instances, gc.HasLen, 1)

	extraKey := "extra-k"
	extraValue := "extra-v"
	err = t.env.(environs.InstanceTagger).TagInstance(
		instances[0].Id(), map[string]string{extraKey: extraValue},
	)
	c.Assert(err, jc.ErrorIsNil)
	assertMetadata(extraKey, extraValue)

	// Ensure that a second call updates existing tags.
	extraValue = "extra-v2"
	err = t.env.(environs.InstanceTagger).TagInstance(
		instances[0].Id(), map[string]string{extraKey: extraValue},
	)
	c.Assert(err, jc.ErrorIsNil)
	assertMetadata(extraKey, extraValue)
}

func prepareParams(attrs map[string]interface{}, cred *identity.Credentials) bootstrap.PrepareParams {
	return bootstrap.PrepareParams{
		ControllerConfig: coretesting.FakeControllerConfig(),
		ModelConfig:      attrs,
		ControllerName:   attrs["name"].(string),
		Cloud:            makeCloudSpec(cred),
		AdminSecret:      testing.AdminSecret,
	}
}

func makeCloudSpec(cred *identity.Credentials) environs.CloudSpec {
	credential := makeCredential(cred)
	return environs.CloudSpec{
		Type:       "openstack",
		Name:       "openstack",
		Endpoint:   cred.URL,
		Region:     cred.Region,
		Credential: &credential,
	}
}

func makeCredential(cred *identity.Credentials) cloud.Credential {
	return cloud.NewCredential(
		cloud.UserPassAuthType,
		map[string]string{
			"username":    cred.User,
			"password":    cred.Secrets,
			"tenant-name": cred.TenantName,
		},
	)
}

// noSwiftSuite contains tests that run against an OpenStack service double
// that lacks Swift.
type noSwiftSuite struct {
	coretesting.BaseSuite
	cred *identity.Credentials
	srv  localServer
	env  environs.Environ
}

func (s *noSwiftSuite) SetUpSuite(c *gc.C) {
	s.BaseSuite.SetUpSuite(c)
	restoreFinishBootstrap := envtesting.DisableFinishBootstrap()
	s.AddCleanup(func(*gc.C) { restoreFinishBootstrap() })

	s.PatchValue(&imagemetadata.SimplestreamsImagesPublicKey, sstesting.SignedMetadataPublicKey)
	s.PatchValue(&keys.JujuPublicKey, sstesting.SignedMetadataPublicKey)
}

func (s *noSwiftSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)
	s.cred = &identity.Credentials{
		User:       "fred",
		Secrets:    "secret",
		Region:     "some-region",
		TenantName: "some tenant",
	}
	s.srv.start(c, s.cred, newNovaOnlyOpenstackService)

	attrs := coretesting.FakeConfig().Merge(coretesting.Attrs{
		"name":            "sample-no-swift",
		"type":            "openstack",
		"auth-mode":       "userpass",
		"agent-version":   coretesting.FakeVersionNumber.String(),
		"authorized-keys": "fakekey",
	})
	s.PatchValue(&jujuversion.Current, coretesting.FakeVersionNumber)
	// Serve fake tools and image metadata using "filestorage",
	// rather than Swift as the rest of the tests do.
	storageDir := c.MkDir()
	imagesDir := filepath.Join(storageDir, "images")
	toolsDir := filepath.Join(storageDir, "tools")
	for _, dir := range []string{imagesDir, toolsDir} {
		err := os.MkdirAll(dir, 0755)
		c.Assert(err, jc.ErrorIsNil)
	}
	toolsStorage, err := filestorage.NewFileStorageWriter(storageDir)
	c.Assert(err, jc.ErrorIsNil)
	envtesting.UploadFakeTools(c, toolsStorage, "released", "released")
	s.PatchValue(&tools.DefaultBaseURL, storageDir)
	imageStorage, err := filestorage.NewFileStorageWriter(imagesDir)
	openstack.UseTestImageData(imageStorage, s.cred)
	imagetesting.PatchOfficialDataSources(&s.CleanupSuite, storageDir)

	env, err := bootstrap.Prepare(
		envtesting.BootstrapContext(c),
		jujuclienttesting.NewMemStore(),
		prepareParams(attrs, s.cred),
	)
	c.Assert(err, jc.ErrorIsNil)
	s.env = env
}

func (s *noSwiftSuite) TearDownTest(c *gc.C) {
	s.srv.stop()
	s.BaseSuite.TearDownTest(c)
}

func (s *noSwiftSuite) TestBootstrap(c *gc.C) {
	err := bootstrap.Bootstrap(envtesting.BootstrapContext(c), s.env, bootstrap.BootstrapParams{
		ControllerConfig: coretesting.FakeControllerConfig(),
		AdminSecret:      testing.AdminSecret,
		CAPrivateKey:     coretesting.CAKey,
	})
	c.Assert(err, jc.ErrorIsNil)
}

func newFullOpenstackService(mux *http.ServeMux, cred *identity.Credentials, auth identity.AuthMode) *novaservice.Nova {
	service := openstackservice.New(cred, auth)
	service.SetupHTTP(mux)
	return service.Nova
}

func newNovaOnlyOpenstackService(mux *http.ServeMux, cred *identity.Credentials, auth identity.AuthMode) *novaservice.Nova {
	var identityService, fallbackService identityservice.IdentityService
	if auth == identity.AuthKeyPair {
		identityService = identityservice.NewKeyPair()
	} else {
		identityService = identityservice.NewUserPass()
		fallbackService = identityservice.NewV3UserPass()
	}
	userInfo := identityService.AddUser(cred.User, cred.Secrets, cred.TenantName)
	if cred.TenantName == "" {
		panic("Openstack service double requires a tenant to be specified.")
	}
	novaService := novaservice.New(cred.URL, "v2", userInfo.TenantId, cred.Region, identityService, fallbackService)
	identityService.SetupHTTP(mux)
	novaService.SetupHTTP(mux)
	return novaService
}

func bootstrapEnv(c *gc.C, env environs.Environ) error {
	return bootstrap.Bootstrap(envtesting.BootstrapContext(c), env, bootstrap.BootstrapParams{
		ControllerConfig: coretesting.FakeControllerConfig(),
		AdminSecret:      testing.AdminSecret,
		CAPrivateKey:     coretesting.CAKey,
	})
}
