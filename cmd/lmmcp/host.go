package main

import (
	"context"
	"github.com/lmctlhq/lmmcp/internal/bridge"
	"github.com/lmctlhq/lmmcp/internal/host"
	"io"
	"os"
	"strings"
)

func hostMode(args []string) bool {
	for _, a := range args {
		if a == "--lmcode" || a == "--lmcode-schema" || a == "--lmcode-signatures" || strings.HasPrefix(a, "--lmcode=") {
			return true
		}
	}
	return false
}
func runHost(ctx context.Context, args []string, out io.Writer) int {
	var files []string
	var config string
	schema, sigs := false, false
	for i := 0; i < len(args); i++ {
		key, value, eq := strings.Cut(args[i], "=")
		switch key {
		case "--json":
			if eq {
				return emit(out, 2, "USAGE")
			}
		case "--lmcode-schema":
			schema = true
		case "--lmcode-signatures":
			sigs = true
		case "--lmcode", "--config":
			if !eq {
				i++
				if i == len(args) {
					return emit(out, 2, "USAGE")
				}
				value = args[i]
			}
			if value == "" {
				return emit(out, 2, "USAGE")
			}
			if key == "--lmcode" {
				files = append(files, value)
			} else {
				config = value
			}
		default:
			return emit(out, 2, "USAGE")
		}
	}
	if schema && sigs || len(files) > 0 && (schema || sigs) {
		return emit(out, 2, "USAGE")
	}
	if schema {
		_, _ = out.Write(host.Schema)
		return 0
	}
	if sigs {
		_, _ = io.WriteString(out, host.Signatures(ctx))
		return 0
	}
	raw, e := os.ReadFile(config)
	if e != nil {
		return emit(out, 2, "CONFIG_UNREADABLE")
	}
	cfg, e := bridge.Load(raw)
	if e != nil {
		return emit(out, 2, "INVALID_CONFIG")
	}
	g, e := bridge.New(cfg, bridge.Options{})
	if e != nil {
		return emit(out, 2, "INVALID_CONFIG")
	}
	defer g.Close()
	return host.Run(files, ctx, g, out)
}
