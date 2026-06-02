package containerd

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/remotes/docker"
	"github.com/xiaods/k8e/pkg/agent/templates"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/rancher/wharfie/pkg/registries"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func init() {
	logrus.SetLevel(logrus.DebugLevel)
}

func u(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

type hostConfigsArgs struct {
	registryContent   string
	noDefaultEndpoint bool
	mirrorAddr        string
}

var hostConfigsTestCases = []struct {
	name string
	args hostConfigsArgs
	want HostConfigs
}{
		{
			name: "no registries",
			want: HostConfigs{},
		},
		{
			name: "registry with default endpoint",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
				`,
			},
			want: HostConfigs{},
		},
		{
			name: "registry with default endpoint explicitly listed",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- docker.io
				`,
			},
			want: HostConfigs{},
		},
		{
			name: "registry with default endpoint - embedded registry",
			args: hostConfigsArgs{
				mirrorAddr: "127.0.0.1:6443",
				registryContent: `
				  mirrors:
						docker.io:
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:6443/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									CAFile:   "server-ca",
									KeyFile:  "client-key",
									CertFile: "client-cert",
								},
							},
						},
					},
				},
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with default endpoint and creds",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
					configs:
					  docker.io:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with default endpoint explicitly listed and creds",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - docker.io
					configs:
					  docker.io:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
			},
		},

		{
			name: "registry with only creds",
			args: hostConfigsArgs{
				registryContent: `
					configs:
					  docker.io:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
			},
		},
		{
			name: "private registry with default endpoint",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						registry.example.com:
				`,
			},
			want: HostConfigs{},
		},
		{
			name: "private registry with default endpoint and creds",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						registry.example.com:
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
			},
		},
		{
			name: "private registry with only creds",
			args: hostConfigsArgs{
				registryContent: `
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - full URL with override path",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
							  - https://registry.example.com/prefix/v2
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							OverridePath: true,
							URL:          u("https://registry.example.com/prefix/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - hostname only with override path",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
							  - registry.example.com/prefix/v2
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							OverridePath: true,
							URL:          u("https://registry.example.com/prefix/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - hostname only with default path",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
							  - registry.example.com/v2
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - full URL",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
							  - https://registry.example.com/v2
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - URL without path",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
							  - https://registry.example.com
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - hostname only",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
							  - registry.example.com
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - hostname and port only",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- registry.example.com:443
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com:443/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - ip address only",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- 1.2.3.4
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://1.2.3.4/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - ip and port only",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- 1.2.3.4:443
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://1.2.3.4:443/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - duplicate endpoints",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- registry.example.com
								- registry.example.com
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - duplicate endpoints in different formats",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- registry.example.com
								- https://registry.example.com
								- https://registry.example.com/v2
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - duplicate endpoints in different positions",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- https://registry.example.com
								- https://registry.example.org
								- https://registry.example.com
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
						},
						{
							URL: u("https://registry.example.org/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - localhost and port only",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- localhost:5000
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("http://localhost:5000/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - localhost and port with scheme",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- https://localhost:5000
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://localhost:5000/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - loopback ip and port only",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- 127.0.0.1:5000
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("http://127.0.0.1:5000/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - loopback ip and port with scheme",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
							endpoint:
								- https://127.0.0.1:5000
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:5000/v2"),
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint and mirror creds",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - https://registry.example.com/v2
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
						},
					},
				},
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint and mirror creds - override path with v2",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - https://registry.example.com/prefix/v2
						registry.example.com:
						  endpoint:
							  - https://registry.example.com/prefix/v2
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							OverridePath: true,
							URL:          u("https://registry.example.com/prefix/v2"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
						},
					},
				},
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							OverridePath: true,
							URL:          u("https://registry.example.com/prefix/v2"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint and mirror creds - override path without v2",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - https://registry.example.com/project/registry
						registry.example.com:
						  endpoint:
							  - https://registry.example.com/project/registry
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							OverridePath: true,
							URL:          u("https://registry.example.com/project/registry"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
						},
					},
				},
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							OverridePath: true,
							URL:          u("https://registry.example.com/project/registry"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint and mirror creds - no default endpoint",
			args: hostConfigsArgs{
				noDefaultEndpoint: true,
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - https://registry.example.com/v2
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
						},
					},
				},
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint and mirror creds - embedded registry",
			args: hostConfigsArgs{
				mirrorAddr: "127.0.0.1:6443",
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - https://registry.example.com/v2
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:6443/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									CAFile:   "server-ca",
									KeyFile:  "client-key",
									CertFile: "client-cert",
								},
							},
						},
						{
							URL: u("https://registry.example.com/v2"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
						},
					},
				},
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint and mirror creds - embedded registry with rewrites",
			args: hostConfigsArgs{
				mirrorAddr: "127.0.0.1:6443",
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - https://registry.example.com/v2
							rewrite:
							  "^rancher/(.*)": "docker/rancher-images/$1"
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:6443/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									CAFile:   "server-ca",
									KeyFile:  "client-key",
									CertFile: "client-cert",
								},
							},
						},
						{
							URL: u("https://registry.example.com/v2"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
							Rewrites: map[string]string{
								"^rancher/(.*)": "docker/rancher-images/$1",
							},
						},
					},
				},
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint and mirror creds - embedded registry and no default endpoint",
			args: hostConfigsArgs{
				mirrorAddr:        "127.0.0.1:6443",
				noDefaultEndpoint: true,
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - https://registry.example.com/v2
					configs:
					  registry.example.com:
						  auth:
							  username: user
								password: pass
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:6443/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									CAFile:   "server-ca",
									KeyFile:  "client-key",
									CertFile: "client-cert",
								},
							},
						},
						{
							URL: u("https://registry.example.com/v2"),
							Config: registries.RegistryConfig{
								Auth: &registries.AuthConfig{
									Username: "user",
									Password: "pass",
								},
							},
						},
					},
				},
				"registry.example.com": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry.example.com/v2"),
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
						},
					},
				},
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - embedded registry, default endpoint explicitly listed",
			args: hostConfigsArgs{
				mirrorAddr: "127.0.0.1:6443",
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - registry.example.com
							  - registry.example.org
								- docker.io
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://registry-1.docker.io/v2"),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:6443/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									CAFile:   "server-ca",
									KeyFile:  "client-key",
									CertFile: "client-cert",
								},
							},
						},
						{
							URL: u("https://registry.example.com/v2"),
						},
						{
							URL: u("https://registry.example.org/v2"),
						},
					},
				},
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},
		{
			name: "registry with mirror endpoint - embedded registry and no default endpoint, default endpoint explicitly listed",
			args: hostConfigsArgs{
				mirrorAddr:        "127.0.0.1:6443",
				noDefaultEndpoint: true,
				registryContent: `
				  mirrors:
						docker.io:
						  endpoint:
							  - registry.example.com
								- registry.example.org
								- docker.io
				`,
			},
			want: HostConfigs{
				"docker.io": templates.HostConfig{
					Program: "k8e",
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:6443/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									CAFile:   "server-ca",
									KeyFile:  "client-key",
									CertFile: "client-cert",
								},
							},
						},
						{
							URL: u("https://registry.example.com/v2"),
						},
						{
							URL: u("https://registry.example.org/v2"),
						},
						{
							URL: u("https://registry-1.docker.io/v2"),
						},
					},
				},
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},
		{
			name: "wildcard mirror endpoint - no endpoints",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						"*":
				`,
			},
			want: HostConfigs{},
		},
		{
			name: "wildcard mirror endpoint - full URL",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						"*":
							endpoint:
								- https://registry.example.com/v2
				`,
			},
			want: HostConfigs{
				"_default": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u(""),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
			},
		},
		{
			name: "wildcard mirror endpoint - full URL, embedded registry",
			args: hostConfigsArgs{
				mirrorAddr: "127.0.0.1:6443",
				registryContent: `
				  mirrors:
						"*":
							endpoint:
								- https://registry.example.com/v2
				`,
			},
			want: HostConfigs{
				"_default": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u(""),
					},
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:6443/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									CAFile:   "server-ca",
									KeyFile:  "client-key",
									CertFile: "client-cert",
								},
							},
						},
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},
		{
			name: "wildcard mirror endpoint - full URL, embedded registry, no default",
			args: hostConfigsArgs{
				noDefaultEndpoint: true,
				mirrorAddr:        "127.0.0.1:6443",
				registryContent: `
				  mirrors:
						"*":
							endpoint:
								- https://registry.example.com/v2
				`,
			},
			want: HostConfigs{
				"_default": templates.HostConfig{
					Program: "k8e",
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://127.0.0.1:6443/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									CAFile:   "server-ca",
									KeyFile:  "client-key",
									CertFile: "client-cert",
								},
							},
						},
						{
							URL: u("https://registry.example.com/v2"),
						},
					},
				},
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},

		{
			name: "wildcard config",
			args: hostConfigsArgs{
				registryContent: `
				  configs:
						"*":
						  auth:
							  username: user
								password: pass
							tls:
								insecure_skip_verify: true
				`,
			},
			want: HostConfigs{
				"_default": {
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						Config: registries.RegistryConfig{
							Auth: &registries.AuthConfig{
								Username: "user",
								Password: "pass",
							},
							TLS: &registries.TLSConfig{
								InsecureSkipVerify: true,
							},
						},
					},
				},
			},
		},
		{
			name: "localhost registry - default https endpoint on unspecified port",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						"localhost":
				`,
			},
			want: HostConfigs{},
		},
		{
			name: "localhost registry - default https endpoint on https port",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						"localhost:443":
				`,
			},
			want: HostConfigs{},
		},
		{
			name: "localhost registry - default http endpoint on odd port",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						"localhost:5000":
				`,
			},
			want: HostConfigs{},
		},
		{
			name: "localhost registry - default http endpoint on http port",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						"localhost:80":
				`,
			},
			want: HostConfigs{},
		},
		{
			name: "localhost registry - default http endpoint on odd port, embedded registry",
			args: hostConfigsArgs{
				mirrorAddr: "127.0.0.1:6443",
				registryContent: `
				  mirrors:
						"localhost:5000":
				`,
			},
			want: HostConfigs{
				// localhost registries are not handled by the embedded registry mirror.
				"127.0.0.1:6443": templates.HostConfig{
					Program: "k8e",
					Default: &templates.RegistryEndpoint{
						URL: u("https://127.0.0.1:6443/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								CAFile:   "server-ca",
								KeyFile:  "client-key",
								CertFile: "client-cert",
							},
						},
					},
				},
			},
		},
		{
			name: "localhost registry - https endpoint on odd port with tls verification disabled",
			args: hostConfigsArgs{
				registryContent: `
				  mirrors:
						localhost:5000:
						  endpoint:
							  - https://localhost:5000
					configs:
						localhost:5000:
						  tls:
							  insecure_skip_verify: true
				`,
			},
			want: HostConfigs{
				"localhost:5000": templates.HostConfig{
					Default: &templates.RegistryEndpoint{
						URL: u("http://localhost:5000/v2"),
						Config: registries.RegistryConfig{
							TLS: &registries.TLSConfig{
								InsecureSkipVerify: true,
							},
						},
					},
					Program: "k8e",
					Endpoints: []templates.RegistryEndpoint{
						{
							URL: u("https://localhost:5000/v2"),
							Config: registries.RegistryConfig{
								TLS: &registries.TLSConfig{
									InsecureSkipVerify: true,
								},
							},
						},
					},
				},
			},
		},
}

func Test_UnitGetHostConfigs(t *testing.T) {
	for _, tt := range hostConfigsTestCases {
		t.Run(tt.name, func(t *testing.T) {
			// replace tabs from the inline yaml with spaces; yaml doesn't support tabs for indentation.
			tt.args.registryContent = strings.ReplaceAll(tt.args.registryContent, "\t", "  ")
			tempDir := t.TempDir()
			registriesFile := filepath.Join(tempDir, "registries.yaml")
			os.WriteFile(registriesFile, []byte(tt.args.registryContent), 0644)
			t.Logf("%s:\n%s", registriesFile, tt.args.registryContent)

			registry, err := registries.GetPrivateRegistries(registriesFile)
			if err != nil {
				t.Fatalf("failed to parse %s: %v\n", registriesFile, err)
			}

			nodeConfig := &config.Node{
				Containerd: config.Containerd{
					Registry: tempDir + "/hosts.d",
				},
				AgentConfig: config.Agent{
					ImageServiceSocket: "containerd-stargz-grpc.sock",
					Registry:           registry.Registry,
					Snapshotter:        "stargz",
				},
			}

		// set up embedded registry mirror TLS config, if enabled for the test
		if tt.args.mirrorAddr != "" {
			if registry.Registry.Configs == nil {
				registry.Registry.Configs = map[string]registries.RegistryConfig{}
			}
			registry.Registry.Configs[tt.args.mirrorAddr] = registries.RegistryConfig{
				TLS: &registries.TLSConfig{
					CAFile:   "server-ca",
					KeyFile:  "client-key",
					CertFile: "client-cert",
				},
			}
			mirrorURL := "https://" + tt.args.mirrorAddr + "/v2"
			if registry.Registry.Mirrors == nil {
				registry.Registry.Mirrors = map[string]registries.Mirror{}
			}
			for host, mirror := range registry.Registry.Mirrors {
				// Don't handle localhost registries (matching old spegel behavior)
				if docker.IsLocalhost(host) {
					continue
				}
				mirror.Endpoints = append([]string{mirrorURL}, mirror.Endpoints...)
				registry.Registry.Mirrors[host] = mirror
			}
		}

			// Generate config template struct for all hosts
			got := getHostConfigs(registry.Registry, tt.args.noDefaultEndpoint, tt.args.mirrorAddr)
			assert.Equal(t, tt.want, got, "getHostConfigs()")

			// Confirm that hosts.toml renders properly for all registries
			for host, config := range got {
				hostsTemplate, err := templates.ParseHostsTemplateFromConfig(templates.HostsTomlTemplate, config)
				assert.NoError(t, err, "ParseHostTemplateFromConfig for %s", host)
				t.Logf("%s/hosts.d/%s/hosts.toml\n%s", tempDir, host, hostsTemplate)
			}

			// Confirm that the main containerd config.toml renders properly
			containerdConfig := templates.ContainerdConfig{
				NodeConfig:            nodeConfig,
				PrivateRegistryConfig: registry.Registry,
				Program:               "k8e",
			}
			configTemplate, err := templates.ParseTemplateFromConfig(templates.ContainerdConfigTemplate, containerdConfig)
			assert.NoError(t, err, "ParseTemplateFromConfig")
			t.Logf("%s/config.toml\n%s", tempDir, configTemplate)
		})
	}
}