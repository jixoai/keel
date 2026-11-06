package durable

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// structuredOutputError explains why a result's structuredOutput breaks the
// tool's contract, or returns "" when it keeps it. Mirrors PI's
// structuredOutputError.
func structuredOutputError(tool *Tool, result ToolResult) string {
	if tool == nil {
		return ""
	}
	schema := tool.StructuredOutputSchema
	value := result.StructuredOutput
	if len(bytes.TrimSpace(schema)) == 0 {
		if value == nil {
			return ""
		}
		return fmt.Sprintf("Tool %s returned structuredOutput but declares no structuredOutputSchema", tool.Name)
	}
	if value == nil {
		if result.IsError {
			return ""
		}
		return fmt.Sprintf("Tool %s returned no structuredOutput", tool.Name)
	}
	sch, err := compileJSONSchema(schema)
	if err != nil {
		return fmt.Sprintf("Tool %s has an uncompilable structuredOutputSchema: %v", tool.Name, err)
	}
	var inst any
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&inst); err != nil {
		return fmt.Sprintf("Tool %s returned structuredOutput that is not valid JSON: %v", tool.Name, err)
	}
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if ok := asValidationError(err, &ve); ok {
			for len(ve.Causes) > 0 {
				ve = ve.Causes[0]
			}
			path := ""
			segments := ve.InstanceLocation
			if len(segments) > 1 {
				segments = segments[1:]
			}
			for _, segment := range segments {
				if path != "" {
					path += "."
				}
				path += strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
			}
			if path == "" {
				path = "root"
			}
			return fmt.Sprintf("Tool %s returned structuredOutput that does not match its schema: %s: %s",
				tool.Name, path, ve.Error())
		}
		return fmt.Sprintf("Tool %s returned structuredOutput that does not match its schema: %v", tool.Name, err)
	}
	return ""
}

func asValidationError(err error, target **jsonschema.ValidationError) bool {
	if ve, ok := err.(*jsonschema.ValidationError); ok {
		*target = ve
		return true
	}
	return false
}
