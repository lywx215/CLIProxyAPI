package management

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

type configV8GeminiGroupSchema struct {
	config.GeminiKey `yaml:",inline"`
	Name             string             `yaml:"name"`
	Keys             []config.GeminiKey `yaml:"keys"`
}

// Existing opaque credential/model options survive management edits. Validate
// the known schema on a copy, accepting only unchanged preexisting extensions;
// the original tree remains the sole source of persisted YAML.
func validateConfigV8WithExistingExtensions(state *configV8Document) error {
	projection := cloneConfigV8Node(state.Root)
	inventory := make(map[string][]any)
	if err := projectConfigV8GeminiExtensions(state.Before, state.Before, inventory, true); err != nil {
		return err
	}
	if err := projectConfigV8GeminiExtensions(projection, state.Before, inventory, false); err != nil {
		return err
	}
	data, err := yaml.Marshal(projection)
	if err != nil {
		return err
	}
	return config.ValidateV8Config(data)
}

func projectConfigV8GeminiExtensions(root, before *yaml.Node, inventory map[string][]any, collect bool) error {
	groups := configV8Node(root, []string{"api-keys", "gemini"})
	previous := configV8Node(before, []string{"api-keys", "gemini"})
	if groups != nil && groups.Kind == yaml.SequenceNode {
		used := make(map[int]bool)
		for index, group := range groups.Content {
			owner := index
			if !collect {
				owner = configV8OriginalGeminiGroup(group, index, previous, used)
			}
			used[owner] = true
			if err := configV8ExtensionProjection(group, reflect.TypeOf(configV8GeminiGroupSchema{}), inventory, collect, fmt.Sprintf("group:%d", owner)); err != nil {
				return err
			}
		}
	}
	return configV8ExtensionProjection(configV8Node(root, []string{"routing", "api-key-model-alias", "gemini"}), reflect.TypeOf([]config.OAuthModelAlias{}), inventory, collect, "aliases")
}

func configV8OriginalGeminiGroup(group *yaml.Node, index int, previous *yaml.Node, used map[int]bool) int {
	if previous == nil || previous.Kind != yaml.SequenceNode {
		return -index - 1
	}
	name := configV8Node(group, []string{"name"})
	keys := configV8Node(group, []string{"keys"})
	best, bestScore := -index-1, 0
	for oldIndex, old := range previous.Content {
		if used[oldIndex] {
			continue
		}
		score := 0
		oldName := configV8Node(old, []string{"name"})
		if name != nil && oldName != nil && name.Value != "" && name.Value == oldName.Value {
			score = 1
		}
		oldKeys := configV8Node(old, []string{"keys"})
		if keys != nil && oldKeys != nil {
			for _, key := range keys.Content {
				credential := configV8Node(key, []string{"api-key"})
				for _, oldKey := range oldKeys.Content {
					oldCredential := configV8Node(oldKey, []string{"api-key"})
					if credential != nil && oldCredential != nil && credential.Value == oldCredential.Value {
						score += 2
						break
					}
				}
			}
		}
		// Retained model/policy options disambiguate empty groups and duplicate
		// names or credentials before falling back to their original position.
		if reflect.DeepEqual(configV8GeminiGroupIdentity(group), configV8GeminiGroupIdentity(old)) {
			score += 2
		}
		if score > 0 && oldIndex == index {
			score++
		}
		if score > bestScore {
			best, bestScore = oldIndex, score
		}
	}
	return best
}

func configV8GeminiGroupIdentity(group *yaml.Node) any {
	copy := cloneConfigV8Node(group)
	for _, field := range []string{"keys", "base-url", "proxy-url", "headers", "priority"} {
		deleteConfigV8Path(copy, []string{field})
	}
	if models := configV8Node(copy, []string{"models"}); models != nil && models.Kind == yaml.SequenceNode {
		for _, model := range models.Content {
			deleteConfigV8Path(model, []string{"alias"})
			deleteConfigV8Path(model, []string{"force-mapping"})
		}
	}
	var identity any
	_ = copy.Decode(&identity)
	return identity
}

func configV8ExtensionProjection(node *yaml.Node, schema reflect.Type, inventory map[string][]any, collect bool, owner string) error {
	if node == nil || node.Tag == "!!null" {
		return nil
	}
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	switch schema.Kind() {
	case reflect.Slice, reflect.Array:
		if node.Kind == yaml.SequenceNode {
			for _, item := range node.Content {
				if err := configV8ExtensionProjection(item, schema.Elem(), inventory, collect, owner); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if node.Kind == yaml.MappingNode {
			for index := 1; index < len(node.Content); index += 2 {
				if err := configV8ExtensionProjection(node.Content[index], schema.Elem(), inventory, collect, owner); err != nil {
					return err
				}
			}
		}
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		fields := configV8SchemaFields(schema)
		if schema == reflect.TypeOf(config.GeminiKey{}) {
			if credential := configV8Node(node, []string{"api-key"}); credential != nil {
				owner += fmt.Sprintf("/key:%x", sha256.Sum256([]byte(credential.Value)))
			}
		} else if schema == reflect.TypeOf(config.GeminiModel{}) || schema == reflect.TypeOf(config.OAuthModelAlias{}) {
			if source := configV8Node(node, []string{"name"}); source != nil {
				owner += "/model:" + source.Value
			}
		}
		for index := 0; index+1 < len(node.Content); {
			field, value := node.Content[index].Value, node.Content[index+1]
			if child, known := fields[field]; known {
				if err := configV8ExtensionProjection(value, child, inventory, collect, owner+"/"+field); err != nil {
					return err
				}
				index += 2
				continue
			}
			var opaque any
			if err := value.Decode(&opaque); err != nil {
				return err
			}
			key := owner + "/" + schema.String() + "." + field
			if collect {
				inventory[key] = append(inventory[key], opaque)
				index += 2
				continue
			}
			unchanged := false
			for oldIndex, old := range inventory[key] {
				if reflect.DeepEqual(old, opaque) {
					unchanged = true
					inventory[key] = append(inventory[key][:oldIndex], inventory[key][oldIndex+1:]...)
					break
				}
			}
			if !unchanged {
				return fmt.Errorf("new or changed unknown Gemini configuration field %s", field)
			}
			node.Content = append(node.Content[:index], node.Content[index+2:]...)
		}
	}
	return nil
}

func configV8SchemaFields(schema reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	for index := 0; index < schema.NumField(); index++ {
		field := schema.Field(index)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("yaml")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		if strings.Contains(tag, ",inline") {
			for name, child := range configV8SchemaFields(field.Type) {
				fields[name] = child
			}
		} else {
			if name == "" {
				name = strings.ToLower(field.Name)
			}
			fields[name] = field.Type
		}
	}
	return fields
}
