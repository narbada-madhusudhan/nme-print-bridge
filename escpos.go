package main

import (
	"encoding/json"
	"log"
	"strings"
)

// ESC/POS command constants — matches src/lib/utils/escpos.ts in admin project
const (
	escInit    = "\x1B@"     // Initialize printer
	escCenter  = "\x1Ba\x01" // Center alignment
	escLeft    = "\x1Ba\x00" // Left alignment
	escRight   = "\x1Ba\x02" // Right alignment
	escLarge   = "\x1D!\x11" // Double height/width
	escNormal  = "\x1D!\x00" // Normal size
	escBoldOn  = "\x1BE\x01" // Bold on
	escBoldOff = "\x1BE\x00" // Bold off
	escCut     = "\x1DV\x00" // Full paper cut
	escFeed    = "\n\n\n\n\n\n" // 6 lines past cutter blade

	receiptWidth = 32
)

// contentToEscPos converts a print job's JSONB content to raw ESC/POS bytes.
// Handles structured format {header, lines[], footer} and plain text {type, text}.
func contentToEscPos(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte("[No content]" + escFeed + escCut)
	}

	var content map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil {
		return []byte("[Invalid content]" + escFeed + escCut)
	}

	// Format 2: Plain text receipt {type, text}
	if textRaw, ok := content["text"]; ok {
		var text string
		if json.Unmarshal(textRaw, &text) == nil && text != "" {
			lines := []string{escInit}
			lines = append(lines, strings.Split(text, "\n")...)
			lines = append(lines, escFeed, escCut)
			return []byte(strings.Join(lines, "\n"))
		}
	}

	// Format 1: Structured PrintContent {header, subheader?, lines[], footer?}
	header := jsonString(content["header"])
	subheader := jsonString(content["subheader"])
	footer := jsonString(content["footer"])

	var lines []printContentLine
	if linesRaw, ok := content["lines"]; ok {
		if err := json.Unmarshal(linesRaw, &lines); err != nil {
			log.Printf("[escpos] Failed to parse lines: %v", err)
		}
	}

	var out []string

	// Initialize + header
	out = append(out, escInit+escCenter+escLarge+header)
	if subheader != "" {
		out = append(out, escNormal+subheader)
	}
	out = append(out, escNormal+escLeft+strings.Repeat("-", receiptWidth))

	// Content lines
	for _, line := range lines {
		if line.IsString {
			out = append(out, line.StringVal)
			continue
		}
		if line.Separator {
			out = append(out, strings.Repeat("-", receiptWidth))
		} else if len(line.Columns) == 2 {
			out = append(out, formatColumns(line.Columns[0], line.Columns[1]))
		} else {
			var prefix, suffix string
			if line.Align == "center" {
				prefix += escCenter
			} else if line.Align == "right" {
				prefix += escRight
			}
			if line.Large {
				prefix += escLarge
				suffix += escNormal
			}
			if line.Bold {
				prefix += escBoldOn
				suffix += escBoldOff
			}
			out = append(out, prefix+line.Text+suffix)
			if line.Align == "center" || line.Align == "right" {
				out = append(out, escLeft)
			}
		}
	}

	// Footer
	out = append(out, strings.Repeat("-", receiptWidth))
	if footer != "" {
		out = append(out, escCenter+footer)
	}
	out = append(out, escFeed, escCut)

	return []byte(strings.Join(out, "\n"))
}

// printContentLine matches the TypeScript PrintContentLine interface.
// Lines can be either strings or structured objects in the JSON array.
type printContentLine struct {
	Text      string   `json:"text"`
	Bold      bool     `json:"bold"`
	Large     bool     `json:"large"`
	Align     string   `json:"align"`
	Separator bool     `json:"separator"`
	Columns   []string `json:"columns"`
	// Set when the JSON array element is a raw string instead of an object
	IsString  bool
	StringVal string
}

func (p *printContentLine) UnmarshalJSON(data []byte) error {
	// Handle plain string elements in the lines array
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		p.IsString = true
		p.StringVal = s
		return nil
	}
	// Structured object
	type alias printContentLine
	return json.Unmarshal(data, (*alias)(p))
}

func formatColumns(left, right string) string {
	padding := receiptWidth - len(left) - len(right)
	if padding > 0 {
		return left + strings.Repeat(" ", padding) + right
	}
	return left + " " + right
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}
