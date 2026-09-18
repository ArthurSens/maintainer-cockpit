package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// JSONSchema returns a schema derived from the configuration structs.
// Config.Parse remains authoritative for cross-field validation that JSON
// Schema cannot express clearly.
func JSONSchema() ([]byte, error) {
	builder := schemaBuilder{definitions: make(map[string]any)}
	root := builder.schemaFor(reflect.TypeFor[Config]())
	schema := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"title":       "Maintainer Cockpit configuration",
		"description": "Run config-check for authoritative cross-field validation.",
		"$ref":        root["$ref"],
		"$defs":       builder.definitions,
	}
	body, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal configuration schema: %w", err)
	}
	return append(body, '\n'), nil
}

type schemaBuilder struct {
	definitions map[string]any
}

func (b *schemaBuilder) schemaFor(typ reflect.Type) map[string]any {
	if typ.Kind() == reflect.Pointer {
		return map[string]any{
			"anyOf": []any{
				b.schemaFor(typ.Elem()),
				map[string]any{"type": "null"},
			},
		}
	}

	switch typ.Kind() {
	case reflect.Struct:
		name := typ.Name()
		if _, exists := b.definitions[name]; !exists {
			b.definitions[name] = map[string]any{}
			b.definitions[name] = b.objectSchema(typ)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	case reflect.Map:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": b.schemaFor(typ.Elem()),
		}
	case reflect.Slice:
		return map[string]any{
			"type":  "array",
			"items": b.schemaFor(typ.Elem()),
		}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	default:
		panic(fmt.Sprintf("unsupported configuration field type %s", typ))
	}
}

func (b *schemaBuilder) objectSchema(typ reflect.Type) map[string]any {
	properties := make(map[string]any)
	var required []string
	for field := range typ.Fields() {
		name, options, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "-" || !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fieldSchema := b.schemaFor(field.Type)
		applySchemaConstraints(typ.Name(), name, fieldSchema)
		properties[name] = fieldSchema
		if !strings.Contains(options, "omitempty") {
			required = append(required, name)
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	if typ == reflect.TypeFor[SecretReference]() {
		schema["oneOf"] = []any{
			map[string]any{"required": []string{"environment"}},
			map[string]any{"required": []string{"file"}},
		}
	}
	return schema
}

func applySchemaConstraints(owner, field string, schema map[string]any) {
	switch owner + "." + field {
	case "Config.external_base_url":
		schema["format"] = "uri"
	case "Config.model_providers":
		schema["propertyNames"] = map[string]any{
			"pattern": "^[a-z][a-z0-9-]{0,62}$",
		}
	case "Config.collections":
		schema["minItems"] = 1
	case "Authorization.organizations":
		schema["uniqueItems"] = true
		schema["items"].(map[string]any)["pattern"] = "^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$"
	case "Authorization.teams":
		schema["uniqueItems"] = true
		schema["items"].(map[string]any)["pattern"] = "^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[a-z0-9](?:[a-z0-9-]{0,99})$"
	case "GitHubAuthenticationOwner.profiles":
		schema["uniqueItems"] = true
		schema["minItems"] = 1
	case "Config.deployment_admins", "Authorization.users":
		schema["uniqueItems"] = true
		schema["items"].(map[string]any)["minimum"] = 1
	case "GitHubAuthentication.profiles":
		schema["minProperties"] = 1
		schema["propertyNames"] = map[string]any{
			"pattern": "^[A-Za-z][A-Za-z0-9-]{0,62}$",
		}
	case "GitHubAuthentication.owners":
		schema["minProperties"] = 1
		schema["propertyNames"] = map[string]any{
			"pattern": "^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$",
		}
	case "GitHubAuthenticationProfile.type":
		schema["enum"] = []string{"github_app", "fine_grained_pat"}
	case "GitHubAuthenticationProfile.app_id",
		"GitHubAuthenticationProfile.installation_id":
		schema["minimum"] = 1
	case "ModelProvider.type":
		schema["enum"] = []string{"openai", "ollama"}
	case "ModelProvider.base_url":
		schema["format"] = "uri"
	case "ModelProvider.model":
		schema["minLength"] = 1
		schema["maxLength"] = 200
	case "GitHub.schedule", "Contribution.schedule", "FeatureCorrelation.schedule",
		"SecretReference.environment", "SecretReference.file",
		"UserAuth.client_id":
		schema["minLength"] = 1
	case "Collection.id":
		schema["pattern"] = "^[a-z][a-z0-9-]{0,62}$"
	case "Collection.name":
		schema["minLength"] = 1
		schema["maxLength"] = 100
	case "Collection.description":
		schema["maxLength"] = 500
	case "Collection.repositories":
		schema["uniqueItems"] = true
		schema["minItems"] = 1
		schema["items"].(map[string]any)["pattern"] = "^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9_.-]{1,100}$"
	case "Collection.discovery":
		schema["propertyNames"] = map[string]any{
			"pattern": "^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9_.-]{1,100}$",
		}
	case "RepositoryDiscovery.mode":
		schema["enum"] = []string{"all_open", "search"}
	case "RepositoryDiscovery.query":
		schema["maxLength"] = 256
	case "DescriptionDiffMismatch.min_references",
		"DescriptionDiffMismatch.strong_mismatch_min_references",
		"BroadAdditionsOnly.min_files", "BroadAdditionsOnly.min_additions",
		"Contribution.established_merged_prs",
		"UnusualActivity.account_age_days", "UnusualActivity.window_days",
		"UnusualActivity.min_repositories", "UnusualActivity.min_organizations":
		schema["minimum"] = 1
	case "BroadAdditionsOnly.max_deletions":
		schema["minimum"] = 0
	}
}
