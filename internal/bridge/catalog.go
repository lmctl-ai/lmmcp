package bridge

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func pages[T any](fetch func(string) ([]T, string, error)) ([]T, error) {
	out := []T{}
	cursor := ""
	seen := map[string]bool{}
	for {
		items, next, err := fetch(cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, bad("INVALID_PAGINATION")
		}
		seen[next] = true
		cursor = next
	}
}
func (g *Gateway) catalog(ctx context.Context, name, kind string) Response {
	switch kind {
	case "resources":
		v, e := pages(func(cursor string) ([]*mcp.Resource, string, error) {
			r, e := operation(g, ctx, name, "list", func(c context.Context, s *mcp.ClientSession) (*mcp.ListResourcesResult, error) {
				return s.ListResources(c, &mcp.ListResourcesParams{Cursor: cursor})
			})
			if e != nil {
				return nil, "", e
			}
			return r.Resources, r.NextCursor, nil
		})
		if e != nil {
			return errorResponse(ctx, e)
		}
		return g.Safe(success(v))
	case "templates":
		v, e := pages(func(cursor string) ([]*mcp.ResourceTemplate, string, error) {
			r, e := operation(g, ctx, name, "list", func(c context.Context, s *mcp.ClientSession) (*mcp.ListResourceTemplatesResult, error) {
				return s.ListResourceTemplates(c, &mcp.ListResourceTemplatesParams{Cursor: cursor})
			})
			if e != nil {
				return nil, "", e
			}
			return r.ResourceTemplates, r.NextCursor, nil
		})
		if e != nil {
			return errorResponse(ctx, e)
		}
		return g.Safe(success(v))
	case "prompts":
		v, e := pages(func(cursor string) ([]*mcp.Prompt, string, error) {
			r, e := operation(g, ctx, name, "list", func(c context.Context, s *mcp.ClientSession) (*mcp.ListPromptsResult, error) {
				return s.ListPrompts(c, &mcp.ListPromptsParams{Cursor: cursor})
			})
			if e != nil {
				return nil, "", e
			}
			return r.Prompts, r.NextCursor, nil
		})
		if e != nil {
			return errorResponse(ctx, e)
		}
		return g.Safe(success(v))
	}
	return failure(404, "NOT_FOUND")
}
func (g *Gateway) read(ctx context.Context, name, uri string) Response {
	if uri == "" {
		return failure(400, "URI_REQUIRED")
	}
	r, e := operation(g, ctx, name, "resource", func(c context.Context, s *mcp.ClientSession) (*mcp.ReadResourceResult, error) {
		return s.ReadResource(c, &mcp.ReadResourceParams{URI: uri})
	})
	if e != nil {
		return errorResponse(ctx, e)
	}
	return g.Safe(success(r))
}
func (g *Gateway) prompt(ctx context.Context, name, prompt string, args map[string]string) Response {
	r, e := operation(g, ctx, name, "prompt", func(c context.Context, s *mcp.ClientSession) (*mcp.GetPromptResult, error) {
		return s.GetPrompt(c, &mcp.GetPromptParams{Name: prompt, Arguments: args})
	})
	if e != nil {
		return errorResponse(ctx, e)
	}
	return g.Safe(success(r))
}
