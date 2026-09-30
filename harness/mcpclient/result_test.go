package mcpclient

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"testing"
)

func TestRenderResult(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("fake image"))
	for _, test := range []struct {
		name   string
		result string
		saved  bool
		want   string
	}{
		{"empty", `{"content":[]}`, false, "The tool returned no content."},
		{"text blocks", `{"content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}`, false, "one\ntwo"},
		{
			"structured content repeated as text",
			`{"content":[{"type":"text","text":"{\"b\": 2, \"a\": 1}"}],"structuredContent":{"a":1,"b":2}}`,
			false, `{"b": 2, "a": 1}`,
		},
		{
			"structured content only",
			`{"content":[],"structuredContent":{"a": [1, 2]}}`,
			false, `{"a":[1,2]}`,
		},
		{"null structured content", `{"content":[{"type":"text","text":"x"}],"structuredContent":null}`, false, "x"},
		{
			"image without a directory",
			`{"content":[{"type":"image","mimeType":"image/png","data":"` + png + `"}]}`,
			false, "[image content (image/png, 10 bytes) not shown]",
		},
		{
			"image saved",
			`{"content":[{"type":"text","text":"see"},{"type":"image","mimeType":"image/png","data":"` + png + `"}]}`,
			true, "see\n[image content (image/png, 10 bytes) saved to /saved/content-2.png]",
		},
		{
			"invalid base64",
			`{"content":[{"type":"audio","mimeType":"audio/wav","data":"%%%"}]}`,
			true, "[audio content (audio/wav) with invalid base64 data]",
		},
		{
			"resource link",
			`{"content":[{"type":"resource_link","uri":"file:///a.txt","name":"a.txt","mimeType":"text/plain","description":"The file."}]}`,
			false, "[resource link: a.txt <file:///a.txt> (text/plain) The file.]",
		},
		{
			"embedded text resource",
			`{"content":[{"type":"resource","resource":{"uri":"memo://1","text":"remember"}}]}`,
			false, "[resource memo://1]\nremember",
		},
		{
			"embedded blob resource",
			`{"content":[{"type":"resource","resource":{"uri":"file:///r.pdf","mimeType":"application/pdf","blob":"` + png + `"}}]}`,
			true, "[resource file:///r.pdf (application/pdf, 10 bytes) saved to /saved/content-1.pdf]",
		},
		{"unknown type", `{"content":[{"type":"hologram","depth":3}]}`, false, `[hologram content: {"type":"hologram","depth":3}]`},
		{"unreadable block", `{"content":[{"type":"text","text":7}]}`, false, `[unreadable content block: {"type":"text","text":7}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var result CallToolResult
			if err := json.Unmarshal([]byte(test.result), &result); err != nil {
				t.Fatal(err)
			}
			var save saveFunc
			if test.saved {
				save = func(name string, data []byte) (string, error) {
					if string(data) != "fake image" {
						return "", errors.New("unexpected data")
					}
					return "/saved/" + name, nil
				}
			}
			if got := renderResult(result, save); got != test.want {
				t.Fatalf("renderResult() = %q, want %q", got, test.want)
			}
		})
	}
}
