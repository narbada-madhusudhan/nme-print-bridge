package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContentToEscPos_Null(t *testing.T) {
	result := contentToEscPos(nil)
	s := string(result)
	if !strings.Contains(s, "[No content]") {
		t.Errorf("expected [No content], got %q", s)
	}
	if !strings.HasSuffix(s, escCut) {
		t.Error("expected paper cut at end")
	}
}

func TestContentToEscPos_NullJSON(t *testing.T) {
	result := contentToEscPos(json.RawMessage(`null`))
	if !strings.Contains(string(result), "[No content]") {
		t.Errorf("expected [No content] for null JSON")
	}
}

func TestContentToEscPos_InvalidJSON(t *testing.T) {
	result := contentToEscPos(json.RawMessage(`{broken`))
	if !strings.Contains(string(result), "[Invalid content]") {
		t.Errorf("expected [Invalid content] for broken JSON")
	}
}

func TestContentToEscPos_PlainText(t *testing.T) {
	content := json.RawMessage(`{"type":"bill","text":"Line 1\nLine 2\nLine 3"}`)
	result := contentToEscPos(content)
	s := string(result)

	if !strings.HasPrefix(s, escInit) {
		t.Error("expected ESC @ init at start")
	}
	if !strings.Contains(s, "Line 1") {
		t.Error("expected Line 1 in output")
	}
	if !strings.Contains(s, "Line 2") {
		t.Error("expected Line 2 in output")
	}
	if !strings.Contains(s, "Line 3") {
		t.Error("expected Line 3 in output")
	}
	if !strings.HasSuffix(s, escCut) {
		t.Error("expected paper cut at end")
	}
}

func TestContentToEscPos_Structured(t *testing.T) {
	content := json.RawMessage(`{
		"header": "KOT #123",
		"subheader": "Table 5",
		"lines": [
			{"text": "2x Chicken Tikka", "bold": true},
			{"separator": true},
			{"columns": ["Subtotal", "Rs 500"]}
		],
		"footer": "Thank you!"
	}`)
	result := contentToEscPos(content)
	s := string(result)

	// Header should have init + center + large
	if !strings.Contains(s, escInit+escCenter+escLarge+"KOT #123") {
		t.Error("expected formatted header")
	}
	// Subheader
	if !strings.Contains(s, escNormal+"Table 5") {
		t.Error("expected subheader")
	}
	// Bold line
	if !strings.Contains(s, escBoldOn+"2x Chicken Tikka"+escBoldOff) {
		t.Error("expected bold line")
	}
	// Separator
	if !strings.Contains(s, strings.Repeat("-", receiptWidth)) {
		t.Error("expected separator line")
	}
	// Columns — "Subtotal" + padding + "Rs 500"
	if !strings.Contains(s, "Subtotal") || !strings.Contains(s, "Rs 500") {
		t.Error("expected columns")
	}
	// Footer
	if !strings.Contains(s, escCenter+"Thank you!") {
		t.Error("expected footer")
	}
	// Feed + cut
	if !strings.Contains(s, escFeed) || !strings.HasSuffix(s, escCut) {
		t.Error("expected feed and cut at end")
	}
}

func TestContentToEscPos_Alignment(t *testing.T) {
	content := json.RawMessage(`{
		"header": "Test",
		"lines": [
			{"text": "centered", "align": "center"},
			{"text": "right", "align": "right"},
			{"text": "large", "large": true}
		]
	}`)
	result := contentToEscPos(content)
	s := string(result)

	if !strings.Contains(s, escCenter+"centered") {
		t.Error("expected center alignment")
	}
	if !strings.Contains(s, escRight+"right") {
		t.Error("expected right alignment")
	}
	if !strings.Contains(s, escLarge+"large"+escNormal) {
		t.Error("expected large text with reset")
	}
	// After center/right lines, should reset to left
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		if strings.Contains(p, "centered") && i+1 < len(parts) {
			if parts[i+1] != escLeft {
				t.Errorf("expected left reset after centered line, got %q", parts[i+1])
			}
		}
	}
}

func TestContentToEscPos_StringLines(t *testing.T) {
	// Lines array can contain raw strings mixed with objects
	content := json.RawMessage(`{
		"header": "Test",
		"lines": [
			"plain string line",
			{"text": "object line"}
		]
	}`)
	result := contentToEscPos(content)
	s := string(result)

	if !strings.Contains(s, "plain string line") {
		t.Error("expected raw string line")
	}
	if !strings.Contains(s, "object line") {
		t.Error("expected object line")
	}
}

func TestContentToEscPos_EmptyLines(t *testing.T) {
	content := json.RawMessage(`{"header": "Header Only"}`)
	result := contentToEscPos(content)
	s := string(result)

	if !strings.Contains(s, "Header Only") {
		t.Error("expected header")
	}
	if !strings.HasSuffix(s, escCut) {
		t.Error("expected cut at end")
	}
}

func TestFormatColumns(t *testing.T) {
	tests := []struct {
		left, right string
		width       int
	}{
		{"Item", "100", receiptWidth},
		{"Long item name that exceeds", "999", 0}, // overflow
	}

	for _, tt := range tests {
		result := formatColumns(tt.left, tt.right)
		if !strings.Contains(result, tt.left) || !strings.Contains(result, tt.right) {
			t.Errorf("formatColumns(%q, %q) = %q, missing content", tt.left, tt.right, result)
		}
		if len(tt.left)+len(tt.right) < receiptWidth {
			if len(result) != receiptWidth {
				t.Errorf("formatColumns(%q, %q) length = %d, want %d", tt.left, tt.right, len(result), receiptWidth)
			}
		}
	}
}

func TestFormatColumns_Overflow(t *testing.T) {
	result := formatColumns("Very long left column text", "Very long right")
	if !strings.Contains(result, " ") {
		t.Error("overflow columns should be separated by space")
	}
}

func TestJsonString(t *testing.T) {
	tests := []struct {
		input    json.RawMessage
		expected string
	}{
		{json.RawMessage(`"hello"`), "hello"},
		{json.RawMessage(`""`), ""},
		{json.RawMessage(`123`), ""},     // not a string
		{json.RawMessage(`null`), ""},    // null
		{nil, ""},                         // empty
		{json.RawMessage(``), ""},         // zero length
	}

	for _, tt := range tests {
		result := jsonString(tt.input)
		if result != tt.expected {
			t.Errorf("jsonString(%s) = %q, want %q", string(tt.input), result, tt.expected)
		}
	}
}

func TestPrintContentLine_UnmarshalJSON_String(t *testing.T) {
	var line printContentLine
	if err := json.Unmarshal([]byte(`"hello world"`), &line); err != nil {
		t.Fatal(err)
	}
	if !line.IsString || line.StringVal != "hello world" {
		t.Errorf("expected IsString=true, StringVal=hello world, got %+v", line)
	}
}

func TestPrintContentLine_UnmarshalJSON_Object(t *testing.T) {
	var line printContentLine
	if err := json.Unmarshal([]byte(`{"text":"item","bold":true,"align":"center"}`), &line); err != nil {
		t.Fatal(err)
	}
	if line.IsString {
		t.Error("expected IsString=false for object")
	}
	if line.Text != "item" || !line.Bold || line.Align != "center" {
		t.Errorf("unexpected line: %+v", line)
	}
}

func TestPrintContentLine_UnmarshalJSON_Columns(t *testing.T) {
	var line printContentLine
	if err := json.Unmarshal([]byte(`{"columns":["Qty","Price"]}`), &line); err != nil {
		t.Fatal(err)
	}
	if len(line.Columns) != 2 || line.Columns[0] != "Qty" || line.Columns[1] != "Price" {
		t.Errorf("unexpected columns: %+v", line)
	}
}
