package containerd

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/remotes/docker"
	"github.com/rancher/wharfie/pkg/registries"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/agent/templates"
	util2 "github.com/xiaods/k8e/pkg/agent/util"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/version"
)

type HostConfigs map[string]templates.HostConfig

// writeContainerdConfig renders and saves config.toml from the filled template
func writeContainerdConfig(cfg *config.Node, containerdConfig templates.ContainerdConfig) error {
	var containerdTemplate string
	containerdTemplateBytes, err := os.ReadFile(cfg.Containerd.Template)
	if err == nil {
		logrus.Infof("Using containerd template at %s", cfg.Containerd.Template)
		containerdTemplate = string(containerdTemplateBytes)
	} else if os.IsNotExist(err) {
		containerdTemplate = templates.ContainerdConfigTemplate
	} else {
		return err
	}
	parsedTemplate, err := templates.ParseTemplateFromConfig(containerdTemplate, containerdConfig)
	if err != nil {
		return err
	}

	return util2.WriteFile(cfg.Containerd.Config, parsedTemplate)
}

// writeContainerdHosts merges registry mirrors/configs, and renders and saves hosts.toml from the filled template
func writeContainerdHosts(cfg *config.Node, containerdConfig templates.ContainerdConfig) error {
	// Embedded registry (spegel P2P) removed — mirror address left empty
	mirrorAddr := ""
	hosts := getHostConfigs(containerdConfig.PrivateRegistryConfig, containerdConfig.NoDefaultEndpoint, mirrorAddr)

	// Clean up previous configuration templates
	if err := cleanContainerdHosts(cfg.Containerd.Registry, hosts); err != nil {
		return err
	}

	// Write out new templates
	for host, config := range hosts {
		hostDir := filepath.Join(cfg.Containerd.Registry, host)
		hostsFile := filepath.Join(hostDir, "hosts.toml")
		hostsTemplate, err := templates.ParseHostsTemplateFromConfig(templates.HostsTomlTemplate, config)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(hostDir, 0700); err != nil {
			return err
		}
		if err := util2.WriteFile(hostsFile, hostsTemplate); err != nil {
			return err
		}
	}

	return nil
}

// cleanContainerdHosts removes any registry host config dirs containing a hosts.toml file
// with a header that indicates it was created by k8e, or directories where a hosts.toml
// is about to be written.  Unmanaged directories not containing this file, or containing
// a file without the header, are left alone.
func cleanContainerdHosts(dir string, hosts HostConfigs) error {
	// clean directories for any registries that we are about to generate a hosts.toml for
	for host := range hosts {
		hostsDir := filepath.Join(dir, host)
		os.RemoveAll(hostsDir)
	}

	// clean directories that contain a hosts.toml with a header indicating it was  created by k8e
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		hostsFile := filepath.Join(dir, ent.Name(), "hosts.toml")
		file, err := os.Open(hostsFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		line, err := bufio.NewReader(file).ReadString('\n')
		if err != nil {
			continue
		}
		if line == templates.HostsTomlHeader {
			hostsDir := filepath.Join(dir, ent.Name())
			os.RemoveAll(hostsDir)
		}
	}

	return nil
}

// getHostConfigs merges the registry mirrors/configs into HostConfig template structs
func getHostConfigs(registry *registries.Registry, noDefaultEndpoint bool, mirrorAddr string) HostConfigs {
	hosts := buildDefaultHostConfigs(registry.Configs, mirrorAddr)

	// create endpoints for mirrors
	for host, mirror := range registry.Mirrors {
		config := ensureMirrorDefaultConfig(hosts, host, mirrorAddr, registry.Configs, noDefaultEndpoint)
		buildMirrorEndpoints(registry.Configs, mirrorAddr, mirror, &config)
		if host == "*" {
			host = "_default"
		}
		hosts[host] = config
	}

	cleanupDefaultHosts(hosts)
	return hosts
}

// buildDefaultHostConfigs creates HostConfig entries for every registry config.
func buildDefaultHostConfigs(configs map[string]registries.RegistryConfig, mirrorAddr string) HostConfigs {
	hosts := map[string]templates.HostConfig{}
	for host, config := range configs {
		if c, err := defaultHostConfig(host, mirrorAddr, config); err != nil {
			logrus.Errorf("Failed to generate config for registry %s: %v", host, err)
		} else {
			if host == "*" {
				host = "_default"
			}
			hosts[host] = *c
		}
	}
	return hosts
}

// ensureMirrorDefaultConfig returns the existing host config or creates one from registry defaults.
func ensureMirrorDefaultConfig(hosts map[string]templates.HostConfig, host, mirrorAddr string, configs map[string]registries.RegistryConfig, noDefaultEndpoint bool) templates.HostConfig {
	if c, ok := hosts[host]; ok {
		return c
	}
	c, err := defaultHostConfig(host, mirrorAddr, configForHost(configs, host))
	if err != nil {
		return templates.HostConfig{}
	}
	if noDefaultEndpoint {
		c.Default = nil
	} else if host == "*" {
		c.Default = &templates.RegistryEndpoint{URL: &url.URL{}}
	}
	return *c
}

// buildMirrorEndpoints populates config.Endpoints from mirror endpoint definitions.
func buildMirrorEndpoints(configs map[string]registries.RegistryConfig, mirrorAddr string, mirror registries.Mirror, config *templates.HostConfig) {
	seenEndpoint := map[string]bool{}
	for i, endpoint := range mirror.Endpoints {
		registryName, epURL, override, err := normalizeEndpointAddress(endpoint, mirrorAddr)
		if err != nil {
			logrus.Warnf("Ignoring invalid endpoint URL %d=%s: %v", i, endpoint, err)
			continue
		}
		if seenEndpoint[epURL.String()] {
			logrus.Warnf("Skipping duplicate endpoint URL %d=%s", i, endpoint)
			continue
		}
		seenEndpoint[epURL.String()] = true
		var rewrites map[string]string
		if epURL.Host != mirrorAddr {
			rewrites = mirror.Rewrites
		}
		ep := templates.RegistryEndpoint{
			Config:       configForHost(configs, registryName),
			Rewrites:     rewrites,
			OverridePath: override,
			URL:          epURL,
		}
		if i+1 == len(mirror.Endpoints) && endpointURLEqual(config.Default, &ep) {
			config.Default = &ep
		} else {
			config.Endpoints = append(config.Endpoints, ep)
		}
	}
}

// cleanupDefaultHosts removes entries that have only unconfigured defaults.
func cleanupDefaultHosts(hosts HostConfigs) {
	for host, config := range hosts {
		if len(config.Endpoints) == 0 && !endpointHasConfig(config.Default) {
			delete(hosts, host)
		}
	}
}

// normalizeEndpointAddress normalizes the endpoint address.
// If successful, it returns the registry name, URL, and a bool indicating if the endpoint path should be overridden.
// If unsuccessful, an error is returned.
// Scheme and hostname logic should match containerd:
// https://github.com/containerd/containerd/blob/v1.7.13/remotes/docker/config/hosts.go#L99-L131
func normalizeEndpointAddress(endpoint, mirrorAddr string) (string, *url.URL, bool, error) {
	// Ensure that the endpoint address has a scheme so that the URL is parsed properly
	if !strings.Contains(endpoint, "://") {
		endpoint = "//" + endpoint
	}
	endpointURL, err := url.Parse(endpoint)
	if err != nil {
		return "", nil, false, err
	}
	port := endpointURL.Port()

	// set default scheme, if not provided
	if endpointURL.Scheme == "" {
		// localhost on odd ports defaults to http, unless it's the embedded mirror
		if docker.IsLocalhost(endpointURL.Host) && port != "" && port != "443" && endpointURL.Host != mirrorAddr {
			endpointURL.Scheme = "http"
		} else {
			endpointURL.Scheme = "https"
		}
	}
	registry := endpointURL.Host
	endpointURL.Host, _ = docker.DefaultHost(registry)
	// This is the reverse of the DefaultHost normalization
	if endpointURL.Host == "registry-1.docker.io" {
		registry = "docker.io"
	}

	switch endpointURL.Path {
	case "", "/", "/v2":
		// If the path is empty, /, or /v2, use the default path.
		endpointURL.Path = "/v2"
		return registry, endpointURL, false, nil
	}

	return registry, endpointURL, true, nil
}

func defaultHostConfig(host, mirrorAddr string, config registries.RegistryConfig) (*templates.HostConfig, error) {
	_, url, _, err := normalizeEndpointAddress(host, mirrorAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint URL %s for %s: %v", host, host, err)
	}
	if host == "*" {
		url = nil
	}
	return &templates.HostConfig{
		Program: version.Program,
		Default: &templates.RegistryEndpoint{
			URL:    url,
			Config: config,
		},
	}, nil
}

func configForHost(configs map[string]registries.RegistryConfig, host string) registries.RegistryConfig {
	// check for config under modified hostname. If the hostname is unmodified, or there is no config for
	// the modified hostname, return the config for the default hostname.
	if h, _ := docker.DefaultHost(host); h != host {
		if c, ok := configs[h]; ok {
			return c
		}
	}
	return configs[host]
}

// endpointURLEqual compares endpoint URL strings
func endpointURLEqual(a, b *templates.RegistryEndpoint) bool {
	var au, bu string
	if a != nil && a.URL != nil {
		au = a.URL.String()
	}
	if b != nil && b.URL != nil {
		bu = b.URL.String()
	}
	return au == bu
}

func endpointHasConfig(ep *templates.RegistryEndpoint) bool {
	if ep != nil {
		return ep.OverridePath || ep.Config.Auth != nil || ep.Config.TLS != nil || len(ep.Rewrites) > 0
	}
	return false
}
