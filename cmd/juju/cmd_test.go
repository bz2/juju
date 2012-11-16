package main

import (
	"io/ioutil"
	"os"
	"reflect"

	"launchpad.net/gnuflag"
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/cmd"
	"launchpad.net/juju-core/environs/dummy"
	"launchpad.net/juju-core/juju/testing"
)

type CmdSuite struct {
	testing.JujuConnSuite
	home fakeHome
}

var _ = Suite(&CmdSuite{})

var envConfig = `
default:
    peckham
environments:
    peckham:
        type: dummy
        state-server: false
        admin-secret: arble
        authorized-keys: i-am-a-key
    walthamstow:
        type: dummy
        state-server: false
        authorized-keys: i-am-a-key
    brokenenv:
        type: dummy
        broken: Bootstrap Destroy
        state-server: false
        authorized-keys: i-am-a-key
`

func (s *CmdSuite) SetUpTest(c *C) {
	s.JujuConnSuite.SetUpTest(c)
	s.home = makeFakeHome(c, "peckham", "walthamstow", "brokenenv")
	err := ioutil.WriteFile(homePath(".juju", "environments.yaml"),
		[]byte(envConfig), 0666)
	c.Assert(err, IsNil)
}

func (s *CmdSuite) TearDownTest(c *C) {
	s.home.restore()
	s.JujuConnSuite.TearDownTest(c)
}

func newFlagSet() *gnuflag.FlagSet {
	return gnuflag.NewFlagSet("", gnuflag.ContinueOnError)
}

// testInit checks that a command initialises correctly
// with the given set of arguments.
func testInit(c *C, com cmd.Command, args []string, errPat string) {
	err := com.Init(newFlagSet(), args)
	if errPat != "" {
		c.Assert(err, ErrorMatches, errPat)
	} else {
		c.Assert(err, IsNil)
	}
}

// assertConnName asserts that the Command is using
// the given environment name.
// Since every command has a different type,
// we use reflection to look at the value of the
// Conn field in the value.
func assertConnName(c *C, com cmd.Command, name string) {
	v := reflect.ValueOf(com).Elem().FieldByName("EnvName")
	c.Assert(v.IsValid(), Equals, true)
	c.Assert(v.Interface(), Equals, name)
}

// All members of EnvironmentInitTests are tested for the -environment and -e
// flags, and that extra arguments will cause parsing to fail.
var EnvironmentInitTests = []func() (cmd.Command, []string){
	func() (cmd.Command, []string) { return new(BootstrapCommand), nil },
	func() (cmd.Command, []string) { return new(DestroyEnvironmentCommand), nil },
	func() (cmd.Command, []string) {
		return new(DeployCommand), []string{"charm-name", "service-name"}
	},
	func() (cmd.Command, []string) { return new(StatusCommand), nil },
}

// TestEnvironmentInit tests that all commands which accept
// the --environment variable initialise their
// environment name correctly.
func (*CmdSuite) TestEnvironmentInit(c *C) {
	for i, cmdFunc := range EnvironmentInitTests {
		c.Logf("test %d", i)
		com, args := cmdFunc()
		testInit(c, com, args, "")
		assertConnName(c, com, "")

		com, args = cmdFunc()
		testInit(c, com, append(args, "-e", "walthamstow"), "")
		assertConnName(c, com, "walthamstow")

		com, args = cmdFunc()
		testInit(c, com, append(args, "--environment", "walthamstow"), "")
		assertConnName(c, com, "walthamstow")

		com, args = cmdFunc()
		testInit(c, com, append(args, "hotdog"), "unrecognized args.*")
	}
}

func runCommand(com cmd.Command, args ...string) (opc chan dummy.Operation, errc chan error) {
	errc = make(chan error, 1)
	opc = make(chan dummy.Operation, 200)
	dummy.Reset()
	dummy.Listen(opc)
	go func() {
		// signal that we're done with this ops channel.
		defer dummy.Listen(nil)

		err := com.Init(newFlagSet(), args)
		if err != nil {
			errc <- err
			return
		}

		err = com.Run(cmd.DefaultContext())
		errc <- err
	}()
	return
}

func (*CmdSuite) TestDestroyEnvironmentCommand(c *C) {
	// normal destroy
	opc, errc := runCommand(new(DestroyEnvironmentCommand))
	c.Check(<-errc, IsNil)
	c.Check((<-opc).(dummy.OpDestroy).Env, Equals, "peckham")

	// destroy with broken environment
	opc, errc = runCommand(new(DestroyEnvironmentCommand), "-e", "brokenenv")
	c.Check(<-opc, IsNil)
	c.Check(<-errc, ErrorMatches, "dummy.Destroy is broken")
	c.Check(<-opc, IsNil)
}

var deployTests = []struct {
	args []string
	com  *DeployCommand
}{
	{
		[]string{"charm-name"},
		&DeployCommand{},
	}, {
		[]string{"charm-name", "service-name"},
		&DeployCommand{ServiceName: "service-name"},
	}, {
		[]string{"--repository", "/path/to/another-repo", "charm-name"},
		&DeployCommand{RepoPath: "/path/to/another-repo"},
	}, {
		[]string{"--upgrade", "charm-name"},
		&DeployCommand{BumpRevision: true},
	}, {
		[]string{"-u", "charm-name"},
		&DeployCommand{BumpRevision: true},
	}, {
		[]string{"--num-units", "33", "charm-name"},
		&DeployCommand{NumUnits: 33},
	}, {
		[]string{"-n", "104", "charm-name"},
		&DeployCommand{NumUnits: 104},
	},
}

func initExpectations(com *DeployCommand) {
	if com.CharmName == "" {
		com.CharmName = "charm-name"
	}
	if com.NumUnits == 0 {
		com.NumUnits = 1
	}
	if com.RepoPath == "" {
		com.RepoPath = "/path/to/repo"
	}
}

func initDeployCommand(args ...string) (*DeployCommand, error) {
	com := &DeployCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestDeployCommandInit(c *C) {
	defer os.Setenv("JUJU_REPOSITORY", os.Getenv("JUJU_REPOSITORY"))
	os.Setenv("JUJU_REPOSITORY", "/path/to/repo")

	for _, t := range deployTests {
		initExpectations(t.com)
		com, err := initDeployCommand(t.args...)
		c.Assert(err, IsNil)
		c.Assert(com, DeepEquals, t.com)
	}

	// test relative --config path
	ctx := &cmd.Context{c.MkDir(), nil, nil, nil}
	expected := []byte("test: data")
	path := ctx.AbsPath("testconfig.yaml")
	file, err := os.Create(path)
	c.Assert(err, IsNil)
	_, err = file.Write(expected)
	c.Assert(err, IsNil)
	file.Close()

	com, err := initDeployCommand("--config", "testconfig.yaml", "charm-name")
	c.Assert(err, IsNil)
	actual, err := com.Config.Read(ctx)
	c.Assert(err, IsNil)
	c.Assert(expected, DeepEquals, actual)

	// missing args
	_, err = initDeployCommand()
	c.Assert(err, ErrorMatches, "no charm specified")

	// bad unit count
	_, err = initDeployCommand("charm-name", "--num-units", "0")
	c.Assert(err, ErrorMatches, "must deploy at least one unit")
	_, err = initDeployCommand("charm-name", "-n", "0")
	c.Assert(err, ErrorMatches, "must deploy at least one unit")

	// environment tested elsewhere
}

func initAddUnitCommand(args ...string) (*AddUnitCommand, error) {
	com := &AddUnitCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestAddUnitCommandInit(c *C) {
	// missing args
	_, err := initAddUnitCommand()
	c.Assert(err, ErrorMatches, "no service specified")

	// bad unit count
	_, err = initAddUnitCommand("service-name", "--num-units", "0")
	c.Assert(err, ErrorMatches, "must add at least one unit")
	_, err = initAddUnitCommand("service-name", "-n", "0")
	c.Assert(err, ErrorMatches, "must add at least one unit")

	// environment tested elsewhere
}

func initExposeCommand(args ...string) (*ExposeCommand, error) {
	com := &ExposeCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestExposeCommandInit(c *C) {
	// missing args
	_, err := initExposeCommand()
	c.Assert(err, ErrorMatches, "no service name specified")

	// environment tested elsewhere
}

func initUnexposeCommand(args ...string) (*UnexposeCommand, error) {
	com := &UnexposeCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestUnexposeCommandInit(c *C) {
	// missing args
	_, err := initUnexposeCommand()
	c.Assert(err, ErrorMatches, "no service name specified")

	// environment tested elsewhere
}

func initSSHCommand(args ...string) (*SSHCommand, error) {
	com := &SSHCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestSSHCommandInit(c *C) {
	// missing args
	_, err := initSSHCommand()
	c.Assert(err, ErrorMatches, "no service name specified")
}

func initSCPCommand(args ...string) (*SCPCommand, error) {
	com := &SCPCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestSCPCommandInit(c *C) {
	// missing args
	_, err := initSCPCommand()
	c.Assert(err, ErrorMatches, "at least two arguments required")

	// not enough args
	_, err = initSCPCommand("mysql/0:foo")
	c.Assert(err, ErrorMatches, "at least two arguments required")
}

func initGetCommand(args ...string) (*GetCommand, error) {
	com := &GetCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestGetCommandInit(c *C) {
	// missing args
	_, err := initGetCommand()
	c.Assert(err, ErrorMatches, "no service name specified")
}

func initSetCommand(args ...string) (*SetCommand, error) {
	com := &SetCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestSetCommandInit(c *C) {
	// missing args
	_, err := initSetCommand()
	c.Assert(err, ErrorMatches, "no service name specified")
	// missing service name
	_, err = initSetCommand("name=cow")
	c.Assert(err, ErrorMatches, "no service name specified")

	// test --config path
	expected := []byte("this: is some test data")
	ctx := &cmd.Context{c.MkDir(), nil, nil, nil}
	path := ctx.AbsPath("testconfig.yaml")
	file, err := os.Create(path)
	c.Assert(err, IsNil)
	_, err = file.Write(expected)
	c.Assert(err, IsNil)
	file.Close()
	com, err := initSetCommand("--config", "testconfig.yaml", "service")
	c.Assert(err, IsNil)
	c.Assert(com.Config.Path, Equals, "testconfig.yaml")
	actual, err := com.Config.Read(ctx)
	c.Assert(err, IsNil)
	c.Assert(actual, DeepEquals, expected)

	// --config path, but no service
	com, err = initSetCommand("--config", "testconfig")
	c.Assert(err, ErrorMatches, "no service name specified")

	// --config and options specified
	com, err = initSetCommand("service", "--config", "testconfig", "bees=")
	c.Assert(err, ErrorMatches, "cannot specify --config when using key=value arguments")
}

func initRemoveUnitCommand(args ...string) (*RemoveUnitCommand, error) {
	com := &RemoveUnitCommand{}
	return com, com.Init(newFlagSet(), args)
}

func (*CmdSuite) TestRemoveUnitCommandInit(c *C) {
	// missing args
	_, err := initRemoveUnitCommand()
	c.Assert(err, ErrorMatches, "no service units specified")
	// not a unit
	_, err = initRemoveUnitCommand("seven/nine")
	c.Assert(err, ErrorMatches, "invalid service unit name: \"seven/nine\"")
}
