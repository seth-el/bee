package bee

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Unknown config fields (G7). The config loader used to reject any field it
// did not know, so an older binary could not start on a newer config and
// every rollback needed the matching config backup too. Now an unknown
// field is ignored with a warning, except:
//
//   - a near miss of a known field at the same level (`requre_plan`,
//     `requirePlan`) still fails, with a "did you mean", because a typo
//     silently ignored is a setting silently not applied;
//   - with "strict_config": true in the config, or BEE_STRICT_CONFIG=1 in
//     the environment, any unknown field fails as before.
//
// `bee doctor` reports ignored fields as a warning (a failure under
// --strict); the CLI and `bee --serve` print them at startup.

// UnknownConfigField is a config key this build does not define.
type UnknownConfigField struct {
	// Path is the dotted location of the key, e.g. "assistant.require_pln".
	Path string
	// Key is the key itself and Parent the dotted path of its object ("" at
	// the top level).
	Key    string
	Parent string
	// Suggestion is a known sibling the key closely resembles, if any.
	Suggestion string
}

// UnknownFields lists config keys that were ignored because this build does
// not know them, redacted and bounded for display (a key is text from the
// file and could itself hold something secret-looking).
func (config Config) UnknownFields() []string {
	out := make([]string, 0, len(config.unknownFields))
	for _, path := range config.unknownFields {
		out = append(out, displayConfigKey(path))
	}
	return out
}

const maxDisplayConfigKeyBytes = 128

func displayConfigKey(key string) string {
	return truncateUTF8(redactString(redactSecretAssignmentsForDisplay(key)), maxDisplayConfigKeyBytes)
}

var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// findUnknownConfigFields walks data against the Go type the config decodes
// into, using encoding/json's own matching (exact, then case-insensitive),
// and returns every key that type does not define, sorted by path.
func findUnknownConfigFields(data []byte, target reflect.Type) []UnknownConfigField {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	var found []UnknownConfigField
	walkConfigValue(value, target, "", &found)
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found
}

func walkConfigValue(value any, target reflect.Type, path string, found *[]UnknownConfigField) {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target == rawMessageType || reflect.PointerTo(target).Implements(jsonUnmarshalerType) || target.Implements(jsonUnmarshalerType) {
		return
	}
	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return
		}
		fields := map[string]reflect.Type{}
		collectJSONFields(target, fields)
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		for key, child := range object {
			fieldType, ok := fields[key]
			if !ok {
				for _, name := range names {
					if strings.EqualFold(name, key) {
						fieldType, ok = fields[name], true
						break
					}
				}
			}
			childPath := joinConfigPath(path, key)
			if !ok {
				*found = append(*found, UnknownConfigField{Path: childPath, Key: key, Parent: path, Suggestion: nearestConfigField(key, names)})
				continue
			}
			walkConfigValue(child, fieldType, childPath, found)
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return
		}
		for key, child := range object {
			walkConfigValue(child, target.Elem(), joinConfigPath(path, key), found)
		}
	case reflect.Slice, reflect.Array:
		if target.Elem().Kind() == reflect.Uint8 {
			return
		}
		items, ok := value.([]any)
		if !ok {
			return
		}
		for index, child := range items {
			walkConfigValue(child, target.Elem(), path+"["+strconv.Itoa(index)+"]", found)
		}
	}
}

// collectJSONFields maps JSON names to field types the way encoding/json
// sees them, promoting untagged embedded structs.
func collectJSONFields(target reflect.Type, fields map[string]reflect.Type) {
	for index := 0; index < target.NumField(); index++ {
		field := target.Field(index)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				collectJSONFields(embedded, fields)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if _, exists := fields[name]; !exists {
			fields[name] = field.Type
		}
	}
}

func joinConfigPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// nearestConfigField returns the known sibling key is most likely a typo
// of: the same name ignoring case, underscores and hyphens (`requirePlan`),
// or a small edit distance (1 for keys up to 5 characters, 2 above).
func nearestConfigField(key string, known []string) string {
	normalized := normalizeConfigKey(key)
	for _, name := range known {
		if normalizeConfigKey(name) == normalized {
			return name
		}
	}
	limit := 2
	if len(key) <= 5 {
		limit = 1
	}
	if len(key) <= 3 {
		return ""
	}
	best, bestDistance := "", limit+1
	for _, name := range known {
		if distance := configEditDistance(strings.ToLower(key), strings.ToLower(name)); distance < bestDistance {
			best, bestDistance = name, distance
		}
	}
	if bestDistance <= limit {
		return best
	}
	return ""
}

func normalizeConfigKey(key string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
}

// configEditDistance is the optimal string alignment distance (Levenshtein
// plus adjacent transpositions, so "tmie" is one edit from "time").
func configEditDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	rows := make([][]int, len(ar)+1)
	for i := range rows {
		rows[i] = make([]int, len(br)+1)
		rows[i][0] = i
	}
	for j := range rows[0] {
		rows[0][j] = j
	}
	for i := 1; i <= len(ar); i++ {
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			rows[i][j] = min(rows[i-1][j]+1, rows[i][j-1]+1, rows[i-1][j-1]+cost)
			if i > 1 && j > 1 && ar[i-1] == br[j-2] && ar[i-2] == br[j-1] {
				rows[i][j] = min(rows[i][j], rows[i-2][j-2]+1)
			}
		}
	}
	return rows[len(ar)][len(br)]
}

// unknownConfigFieldError is the load error for a near miss, or for any
// unknown field in strict mode.
func unknownConfigFieldError(fields []UnknownConfigField, strict bool) error {
	var messages []string
	for _, field := range fields {
		if field.Suggestion == "" && !strict {
			continue
		}
		message := fmt.Sprintf("unknown field %q", displayConfigKey(field.Key))
		if field.Parent != "" {
			message += " in " + displayConfigKey(field.Parent)
		}
		if field.Suggestion != "" {
			message += fmt.Sprintf(" (did you mean %q?)", field.Suggestion)
		}
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return nil
	}
	if len(messages) > 5 {
		messages = append(messages[:5], fmt.Sprintf("and %d more", len(messages)-5))
	}
	return fmt.Errorf("json: %s", strings.Join(messages, "; "))
}

func strictConfigFromEnvironment() bool {
	value := strings.TrimSpace(os.Getenv("BEE_STRICT_CONFIG"))
	return value == "1" || strings.EqualFold(value, "true")
}

// decodeConfigJSONLenient is decodeConfigJSON without DisallowUnknownFields:
// duplicate keys and trailing data are still rejected.
func decodeConfigJSONLenient(data []byte, value any) error {
	if err := rejectConfigDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("config has trailing JSON data")
		}
		return err
	}
	return nil
}

// UnknownConfigFieldWarnings renders one warning line per ignored field,
// for printing on stderr at startup.
func UnknownConfigFieldWarnings(config Config) []string {
	fields := config.UnknownFields()
	lines := make([]string, 0, len(fields))
	for _, path := range fields {
		lines = append(lines, fmt.Sprintf("warning: config: ignoring unknown field %q (this Bee build does not support it; set \"strict_config\": true to make this an error)", path))
	}
	return lines
}
