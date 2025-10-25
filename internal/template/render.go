package template

import (
    "bytes"
    "fmt"
    texttpl "text/template"
)

// RenderCommandTemplates renders a slice of Go text/template strings with the given data context.
// It enforces missingkey=error so templates cannot access absent fields.
func RenderCommandTemplates(cmdTemplates []string, data any) ([]string, error) {
    out := make([]string, 0, len(cmdTemplates))
    for i, s := range cmdTemplates {
        t, err := texttpl.New(fmt.Sprintf("cmd_%d", i)).Option("missingkey=error").Parse(s)
        if err != nil {
            return nil, fmt.Errorf("parse template %d: %w", i, err)
        }
        var buf bytes.Buffer
        if err := t.Execute(&buf, data); err != nil {
            return nil, fmt.Errorf("execute template %d: %w", i, err)
        }
        out = append(out, buf.String())
    }
    return out, nil
}

