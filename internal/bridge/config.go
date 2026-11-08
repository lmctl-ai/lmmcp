package bridge

import (
	"encoding/json"
	"strings"
)

type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}
type ServerConfig struct {
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	EnvAllowlist    []string          `json:"envAllowlist,omitempty"`
	Cwd             string            `json:"cwd,omitempty"`
	URL             string            `json:"url,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Transport       string            `json:"transport,omitempty"`
	ProtocolVersion string            `json:"protocolVersion,omitempty"`
	Type            string            `json:"type,omitempty"`
	Name            string            `json:"name,omitempty"`
	Description     string            `json:"description,omitempty"`
	Disabled        bool              `json:"disabled,omitempty"`
	AllowTools      []string          `json:"allowTools,omitempty"`
	DenyTools       []string          `json:"denyTools,omitempty"`
}

func envName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func expand(s string, lookup func(string) (string, bool)) (string, []string, error) {
	var out strings.Builder
	var secrets []string
	for {
		at := strings.Index(s, "${")
		if at < 0 {
			out.WriteString(s)
			break
		}
		out.WriteString(s[:at])
		rest := s[at+2:]
		end := strings.IndexByte(rest, '}')
		if end < 0 || !envName(rest[:end]) {
			return "", nil, bad("INVALID_ENV_REFERENCE")
		}
		v, ok := lookup(rest[:end])
		if !ok {
			return "", nil, bad("ENV_NOT_SET")
		}
		out.WriteString(v)
		if v != "" {
			secrets = append(secrets, v)
		}
		s = rest[end+1:]
	}
	return out.String(), secrets, nil
}
func Load(data []byte) (Config, error) {
	var root map[string]json.RawMessage
	if e := Decode(data, &root); e != nil {
		return Config{}, e
	}
	var cfg Config
	if root["mcpServers"] == nil {
		return cfg, bad("INVALID_CONFIG")
	}
	if e := Decode(root["mcpServers"], &cfg.Servers); e != nil || cfg.Servers == nil {
		return cfg, bad("INVALID_CONFIG")
	}
	for name, c := range cfg.Servers {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\r\n\x00") {
			return cfg, bad("INVALID_CONFIG")
		}
		if (c.Command == "") == (c.URL == "") {
			return cfg, bad("INVALID_CONFIG")
		}
		if strings.ContainsRune(c.Command, 0) || strings.ContainsRune(c.Cwd, 0) {
			return cfg, bad("INVALID_CONFIG")
		}
		if err := validateRemote(c); err != nil {
			return cfg, err
		}
		for _, a := range c.Args {
			if strings.ContainsRune(a, 0) {
				return cfg, bad("INVALID_CONFIG")
			}
		}
		for _, tool := range append(append([]string(nil), c.AllowTools...), c.DenyTools...) {
			if tool == "" || strings.ContainsRune(tool, 0) {
				return cfg, bad("INVALID_CONFIG")
			}
		}
		for k, v := range c.Env {
			if !envName(k) || strings.ContainsRune(v, 0) {
				return cfg, bad("INVALID_CONFIG")
			}
			if _, _, e := expand(v, func(string) (string, bool) { return "fixture", true }); e != nil {
				return cfg, e
			}
		}
		for _, k := range c.EnvAllowlist {
			if !envName(k) {
				return cfg, bad("INVALID_CONFIG")
			}
		}
	}
	return cfg, nil
}
