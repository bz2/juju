package dummy_test

import (
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/config"
	"launchpad.net/juju-core/testing"
	_ "launchpad.net/juju-core/environs/dummy"
)

var _ = Suite(&ConfigSuite{})

type ConfigSuite struct{}

func (*ConfigSuite) TestSecretAttrs(c *C) {
	cfg, err := config.New(map[string]interface{}{
		"name":            "only", // must match the name in environs_test.go
		"type":            "dummy",
		"state-server":    true,
		"authorized-keys": "i-am-a-key",
		"root-cert": testing.RootCertPEM,
		"root-private-key": "",
	})
	c.Assert(err, IsNil)
	env, err := environs.New(cfg)
	c.Assert(err, IsNil)
	expected := map[string]interface{}{
		"secret": "pork",
	}
	actual, err := env.Provider().SecretAttrs(cfg)
	c.Assert(err, IsNil)
	c.Assert(expected, DeepEquals, actual)
}

var firewallModeTests = []struct {
	configFirewallMode string
	firewallMode       config.FirewallMode
	errorMsg           string
}{
	{
		// Empty value leads to default value.
		firewallMode: config.FwInstance,
	}, {
		// Explicit default value.
		configFirewallMode: "",
		firewallMode:       config.FwInstance,
	}, {
		// Instance mode.
		configFirewallMode: "instance",
		firewallMode:       config.FwInstance,
	}, {
		// Global mode.
		configFirewallMode: "global",
		firewallMode:       config.FwGlobal,
	}, {
		// Invalid mode.
		configFirewallMode: "invalid",
		errorMsg:           `invalid firewall mode in environment configuration: "invalid"`,
	},
}

func (*ConfigSuite) TestFirewallMode(c *C) {
	for _, test := range firewallModeTests {
		c.Logf("test firewall mode %q", test.configFirewallMode)
		cfgMap := map[string]interface{}{
			"name":         "only",
			"type":         "dummy",
			"state-server": true,
			"authorized-keys": "none",
			"root-cert": testing.RootCertPEM,
			"root-private-key": "",
		}
		if test.configFirewallMode != "" {
			cfgMap["firewall-mode"] = test.configFirewallMode
		}
		cfg, err := config.New(cfgMap)
		if err != nil {
			c.Assert(err, ErrorMatches, test.errorMsg)
			continue
		}

		env, err := environs.New(cfg)
		if err != nil {
			c.Assert(err, ErrorMatches, test.errorMsg)
			continue
		}

		firewallMode := env.Config().FirewallMode()
		c.Assert(firewallMode, Equals, test.firewallMode)
	}
}
