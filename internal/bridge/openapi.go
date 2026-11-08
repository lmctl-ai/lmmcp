package bridge

import "encoding/json"

const spec = `{"openapi":"3.1.0","info":{"title":"lmmcp slice 1","version":"0.1.0"},"security":[{"bearerAuth":[]}],"components":{"securitySchemes":{"bearerAuth":{"type":"http","scheme":"bearer"}},"schemas":{"Envelope":{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"},"data":{},"error":{"type":"object","required":["code","message"],"properties":{"code":{"type":"string"},"message":{"type":"string"}}}}}}},"x-next-iteration":["public packaging","native platform qualification"],"paths":{}}`

func OpenAPI() map[string]any {
	var doc map[string]any
	_ = json.Unmarshal([]byte(spec), &doc)
	paths := map[string]any{}
	for path, method := range map[string]string{"/health": "get", "/v1/openapi.json": "get", "/v1/servers": "get", "/v1/servers/{server}/tools": "get", "/v1/servers/{server}/tools/{tool}/call": "post", "/v1/requests/{id}": "delete", "/v1/servers/{server}/resources": "get", "/v1/servers/{server}/resources/read": "get", "/v1/servers/{server}/resources/templates": "get", "/v1/servers/{server}/prompts": "get", "/v1/servers/{server}/prompts/{prompt}": "post"} {
		parameters := []any{}
		for _, n := range []string{"server", "tool", "id", "prompt"} {
			if containsSegment(path, "{"+n+"}") {
				parameters = append(parameters, map[string]any{"name": n, "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
			}
		}
		parameters = append(parameters, map[string]any{"name": "X-Request-ID", "in": "header", "schema": map[string]string{"type": "string"}})
		if path == "/v1/servers/{server}/resources/read" {
			parameters = append(parameters, map[string]any{"name": "uri", "in": "query", "required": true, "schema": map[string]string{"type": "string"}})
		}
		envelopeRef := map[string]any{"application/json": map[string]any{"schema": map[string]string{"$ref": "#/components/schemas/Envelope"}}}
		op := map[string]any{"parameters": parameters, "responses": map[string]any{"200": map[string]any{"description": "Complete JSON result; tool isError returns HTTP 200 with ok=false, error.code=TOOL_ERROR and original MCP data (CLI exit 3)", "content": envelopeRef}, "default": map[string]any{"description": "JSON error with code/message; 400 invalid, 401 auth, 403 policy, 404 absent, 409 duplicate id, 499 cancelled, 502 upstream", "content": envelopeRef}}}
		if path == "/v1/openapi.json" {
			op["responses"].(map[string]any)["200"].(map[string]any)["content"] = map[string]any{"application/json": map[string]any{"schema": map[string]string{"type": "object"}}}
		}
		if method == "post" {
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object", "additionalProperties": true}}}}
		}
		paths[path] = map[string]any{method: op}
	}
	doc["paths"] = paths
	return doc
}
func containsSegment(path, value string) bool {
	for i := 0; i+len(value) <= len(path); i++ {
		if path[i:i+len(value)] == value {
			return true
		}
	}
	return false
}
