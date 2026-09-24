package nodesettings

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

func pointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func checkFields(raw json.RawMessage, path string, fields ...string) []ValidationError {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return []ValidationError{{Path: path, Code: "invalid_type"}}
	}
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	errors := []ValidationError{}
	for field := range object {
		if !allowed[field] {
			errors = append(errors, ValidationError{Path: path + "/" + pointerToken(field), Code: "unknown_field"})
		}
	}
	return errors
}

// ValidateMCPDocumentJSON validates an unsaved document without reading state or contacting endpoints.
func ValidateMCPDocumentJSON(raw json.RawMessage) MCPValidation {
	result := MCPValidation{SchemaID: "harness-mcp-validation-v2", Errors: []ValidationError{}}
	result.Errors = append(result.Errors, checkFields(raw, "/mcpDocument", "schemaId", "servers")...)
	var document struct {
		SchemaID string            `json:"schemaId"`
		Servers  []json.RawMessage `json:"servers"`
	}
	if json.Unmarshal(raw, &document) != nil {
		result.Errors = append(result.Errors, ValidationError{Path: "/mcpDocument", Code: "invalid_type"})
		return result
	}
	if document.SchemaID != MCPDocumentSchemaID {
		result.Errors = append(result.Errors, ValidationError{Path: "/mcpDocument/schemaId", Code: "unsupported_schema"})
	}
	if document.Servers == nil || len(document.Servers) > maximumServers {
		result.Errors = append(result.Errors, ValidationError{Path: "/mcpDocument/servers", Code: "invalid_servers"})
	}
	seen := map[string]bool{}
	for index, serverRaw := range document.Servers {
		base := fmt.Sprintf("/mcpDocument/servers/%d", index)
		before := len(result.Errors)
		result.Errors = append(result.Errors, checkFields(serverRaw, base, "id", "name", "enabled", "transport", "url", "timeoutMs", "auth")...)
		var object map[string]json.RawMessage
		_ = json.Unmarshal(serverRaw, &object)
		if authRaw, ok := object["auth"]; ok {
			result.Errors = append(result.Errors, checkFields(authRaw, base+"/auth", "kind", "secretAction", "secret", "bearerTokenConfigured")...)
		}
		var server MCPServer
		if json.Unmarshal(serverRaw, &server) != nil {
			if len(result.Errors) == before {
				result.Errors = append(result.Errors, ValidationError{Path: base, Code: "invalid_type"})
			}
			continue
		}
		if !identifierPattern.MatchString(server.ID) || seen[server.ID] {
			result.Errors = append(result.Errors, ValidationError{Path: base + "/id", Code: "invalid_id"})
		}
		seen[server.ID] = true
		if !validText(server.Name, 200) {
			result.Errors = append(result.Errors, ValidationError{Path: base + "/name", Code: "invalid_name"})
		}
		if server.Transport != "streamable_http" && server.Transport != "sse" {
			result.Errors = append(result.Errors, ValidationError{Path: base + "/transport", Code: "unsupported_transport"})
		}
		parsed, err := url.Parse(server.URL)
		if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			result.Errors = append(result.Errors, ValidationError{Path: base + "/url", Code: "invalid_url"})
		}
		if server.TimeoutMS < 100 || server.TimeoutMS > 120000 {
			result.Errors = append(result.Errors, ValidationError{Path: base + "/timeoutMs", Code: "invalid_timeout"})
		}
		for _, required := range []string{"id", "name", "enabled", "transport", "url", "timeoutMs"} {
			if _, ok := object[required]; !ok {
				result.Errors = append(result.Errors, ValidationError{Path: base + "/" + required, Code: "required"})
			}
		}
		if _, ok := object["auth"]; ok {
			if server.Auth.Kind != "none" && server.Auth.Kind != "bearer" {
				result.Errors = append(result.Errors, ValidationError{Path: base + "/auth/kind", Code: "unsupported_auth"})
			}
			if server.Auth.SecretAction != "keep" && server.Auth.SecretAction != "replace" && server.Auth.SecretAction != "remove" {
				result.Errors = append(result.Errors, ValidationError{Path: base + "/auth/secretAction", Code: "invalid_secret_action"})
			}
			if server.Auth.Kind == "none" && (server.Auth.SecretAction != "remove" || server.Auth.Secret != "") {
				result.Errors = append(result.Errors, ValidationError{Path: base + "/auth", Code: "invalid_auth"})
			}
			if server.Auth.Kind == "bearer" && ((server.Auth.SecretAction == "keep" && server.Auth.Secret != "") || (server.Auth.SecretAction == "replace" && (!validText(server.Auth.Secret, 16<<10) || strings.ContainsAny(server.Auth.Secret, "\r\n")))) {
				result.Errors = append(result.Errors, ValidationError{Path: base + "/auth", Code: "invalid_auth"})
			}
		} else {
			result.Errors = append(result.Errors, ValidationError{Path: base + "/auth", Code: "required"})
		}
	}
	sort.Slice(result.Errors, func(i, j int) bool { return result.Errors[i].Path < result.Errors[j].Path })
	result.Valid = len(result.Errors) == 0
	return result
}

// ValidatePutJSON locates unknown fields before Go's typed decoder can discard their path.
func ValidatePutJSON(raw []byte) []ValidationError {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return []ValidationError{{Path: "", Code: "invalid_type"}}
	}
	errors := checkFields(raw, "", "expectedRevision", "draft")
	if _, ok := root["expectedRevision"]; !ok {
		errors = append(errors, ValidationError{Path: "/expectedRevision", Code: "required"})
	}
	draftRaw, ok := root["draft"]
	if !ok {
		return append(errors, ValidationError{Path: "/draft", Code: "required"})
	}
	errors = append(errors, checkFields(draftRaw, "/draft", "inference", "mcpDocument", "mcpServers")...)
	var draft map[string]json.RawMessage
	if json.Unmarshal(draftRaw, &draft) != nil || draft == nil {
		return append(errors, ValidationError{Path: "/draft", Code: "invalid_type"})
	}
	if inference, ok := draft["inference"]; ok {
		errors = append(errors, checkFields(inference, "/draft/inference", "modelId", "speedMode", "reasoningEffort")...)
	} else {
		errors = append(errors, ValidationError{Path: "/draft/inference", Code: "required"})
	}
	if document, ok := draft["mcpDocument"]; ok {
		validated := ValidateMCPDocumentJSON(document)
		for _, issue := range validated.Errors {
			issue.Path = "/draft" + issue.Path
			errors = append(errors, issue)
		}
	} else if _, legacy := draft["mcpServers"]; !legacy {
		errors = append(errors, ValidationError{Path: "/draft/mcpDocument", Code: "required"})
	}
	if legacy, ok := draft["mcpServers"]; ok {
		var servers []json.RawMessage
		if json.Unmarshal(legacy, &servers) != nil || servers == nil {
			errors = append(errors, ValidationError{Path: "/draft/mcpServers", Code: "invalid_type"})
		} else {
			for index, server := range servers {
				base := fmt.Sprintf("/draft/mcpServers/%d", index)
				errors = append(errors, checkFields(server, base, "id", "name", "enabled", "transport", "url", "timeoutMs", "auth")...)
				var item map[string]json.RawMessage
				if json.Unmarshal(server, &item) == nil {
					if auth, ok := item["auth"]; ok {
						errors = append(errors, checkFields(auth, base+"/auth", "kind", "secretAction", "secret", "bearerTokenConfigured")...)
					}
				}
			}
		}
	}
	sort.Slice(errors, func(i, j int) bool { return errors[i].Path < errors[j].Path })
	return errors
}
