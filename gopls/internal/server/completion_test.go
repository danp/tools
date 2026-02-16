// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"testing"
	"time"

	"golang.org/x/tools/gopls/internal/file"
	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/settings"
)

func TestFallbackProtocolItemsIgnoresCommentsAndStrings(t *testing.T) {
	content := []byte(`package p

func _() {
	fooValue := 1
	// foComment
	_ = "foString"
	fo
}
`)
	fh := fakeHandle{
		uri:     protocol.URIFromPath("/tmp/p.go"),
		content: content,
	}

	offset := 0
	for i := len(content) - 1; i > 0; i-- {
		if content[i] == '\n' && content[i-1] == 'o' && content[i-2] == 'f' {
			offset = i
			break
		}
	}
	if offset == 0 {
		t.Fatal("failed to locate completion offset")
	}
	mapper := protocol.NewMapper(fh.URI(), content)
	pos, err := mapper.OffsetPosition(offset)
	if err != nil {
		t.Fatalf("OffsetPosition: %v", err)
	}

	items, err := fallbackProtocolItems(fh, pos, &settings.Options{})
	if err != nil {
		t.Fatalf("fallbackProtocolItems: %v", err)
	}
	labels := make(map[string]bool)
	for _, item := range items {
		labels[item.Label] = true
	}
	if !labels["fooValue"] {
		t.Fatalf("missing expected identifier fooValue in fallback items: %#v", items)
	}
	if labels["foComment"] {
		t.Fatalf("unexpected comment identifier foComment in fallback items: %#v", items)
	}
	if labels["foString"] {
		t.Fatalf("unexpected string identifier foString in fallback items: %#v", items)
	}
}

type fakeHandle struct {
	uri     protocol.DocumentURI
	content []byte
}

func (h fakeHandle) URI() protocol.DocumentURI   { return h.uri }
func (h fakeHandle) Version() int32              { return 0 }
func (h fakeHandle) SameContentsOnDisk() bool    { return true }
func (h fakeHandle) String() string              { return h.uri.Path() }
func (h fakeHandle) Content() ([]byte, error)    { return h.content, nil }
func (h fakeHandle) ModTime() (time.Time, error) { return time.Time{}, nil }
func (h fakeHandle) Identity() file.Identity {
	return file.Identity{URI: h.uri, Hash: file.HashOf(h.content)}
}
