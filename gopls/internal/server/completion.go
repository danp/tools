// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"fmt"
	"go/scanner"
	"go/token"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/gopls/internal/cache"
	"golang.org/x/tools/gopls/internal/cache/parsego"
	"golang.org/x/tools/gopls/internal/file"
	"golang.org/x/tools/gopls/internal/golang"
	"golang.org/x/tools/gopls/internal/golang/completion"
	"golang.org/x/tools/gopls/internal/label"
	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/settings"
	"golang.org/x/tools/gopls/internal/telemetry"
	"golang.org/x/tools/gopls/internal/template"
	"golang.org/x/tools/gopls/internal/work"
	"golang.org/x/tools/internal/event"
)

func (s *server) Completion(ctx context.Context, params *protocol.CompletionParams) (_ *protocol.CompletionList, rerr error) {
	recordLatency := telemetry.StartLatencyTimer("completion")
	defer func() {
		recordLatency(ctx, rerr)
	}()

	ctx, done := event.Start(ctx, "server.Completion", label.URI.Of(params.TextDocument.URI))
	defer done()

	fh, snapshot, release, err := s.session.FileOf(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	defer release()

	if params.Range.Start != params.Range.End {
		return nil, fmt.Errorf("textDocument/completion request only applicable for position")
	}
	pos := params.Range.Start

	var candidates []completion.CompletionItem
	var surrounding *completion.Selection
	switch snapshot.FileKind(fh) {
	case file.Go:
		candidates, surrounding, err = completion.Completion(ctx, snapshot, fh, pos, params.Context)
	case file.Mod:
		candidates, surrounding = nil, nil
	case file.Work:
		cl, err := work.Completion(ctx, snapshot, fh, pos)
		if err != nil {
			break
		}
		return cl, nil
	case file.Tmpl:
		var cl *protocol.CompletionList
		cl, err = template.Completion(ctx, snapshot, fh, pos, params.Context)
		if err != nil {
			break // use common error handling, candidates==nil
		}
		return cl, nil
	}
	if err != nil {
		event.Error(ctx, "no completions found", err, label.Position.Of(pos))
	}
	if candidates == nil || surrounding == nil {
		if snapshot.FileKind(fh) == file.Go {
			// Only synthesize lexical fallback completions when normal completion
			// returned no result without an explicit error, and the file has
			// parse errors (recovery mode).
			if err == nil && fileHasParseErrors(ctx, snapshot, fh) {
				if fallback, err := fallbackProtocolItems(fh, pos, snapshot.Options()); err == nil && len(fallback) > 0 {
					return &protocol.CompletionList{
						IsIncomplete: true,
						Items:        fallback,
					}, nil
				}
			}
		}
		complEmpty.Inc()
		return &protocol.CompletionList{
			IsIncomplete: true,
			Items:        []protocol.CompletionItem{},
		}, nil
	}

	// When using deep completions/fuzzy matching, report results as incomplete so
	// client fetches updated completions after every key stroke.
	options := snapshot.Options()
	incompleteResults := options.DeepCompletion || options.Matcher == settings.Fuzzy

	items, err := toProtocolCompletionItems(candidates, surrounding, options)
	if err != nil {
		return nil, err
	}
	if snapshot.FileKind(fh) == file.Go {
		s.saveLastCompletion(fh.URI(), fh.Version(), items, params.Position)
	}

	if len(items) > 10 {
		// TODO(pjw): long completions are ok for field lists
		complLong.Inc()
	} else {
		complShort.Inc()
	}
	return &protocol.CompletionList{
		IsIncomplete: incompleteResults,
		Items:        items,
	}, nil
}

func fileHasParseErrors(ctx context.Context, snapshot *cache.Snapshot, fh file.Handle) bool {
	pgf, err := snapshot.ParseGo(ctx, fh, parsego.Full)
	return err == nil && pgf.ParseErr != nil
}

func fallbackProtocolItems(fh file.Handle, pos protocol.Position, options *settings.Options) ([]protocol.CompletionItem, error) {
	content, err := fh.Content()
	if err != nil {
		return nil, err
	}
	mapper := protocol.NewMapper(fh.URI(), content)
	offset, err := mapper.PositionOffset(pos)
	if err != nil || offset < 0 || offset > len(content) {
		return nil, nil
	}
	prefixStart := identPrefixStart(content, offset)
	if prefixStart == offset {
		return nil, nil
	}
	prefix := string(content[prefixStart:offset])
	if prefix == "" {
		return nil, nil
	}

	startPos, err := mapper.OffsetPosition(prefixStart)
	if err != nil {
		return nil, err
	}
	rng := protocol.Range{Start: startPos, End: pos}

	ids := lexicalIdentifiers(content)
	seen := make(map[string]bool)
	items := make([]protocol.CompletionItem, 0, 8)
	for _, id := range ids {
		if id == prefix || !strings.HasPrefix(id, prefix) || seen[id] {
			continue
		}
		seen[id] = true
		item := protocol.CompletionItem{
			Label:            id,
			Kind:             protocol.VariableCompletion,
			SortText:         id,
			FilterText:       id,
			InsertTextFormat: &options.InsertTextFormat,
		}
		item.TextEdit = &protocol.Or_CompletionItem_textEdit{
			Value: protocol.TextEdit{
				NewText: id,
				Range:   rng,
			},
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
	return items, nil
}

func lexicalIdentifiers(content []byte) []string {
	var (
		s    scanner.Scanner
		fset = token.NewFileSet()
		file = fset.AddFile("", -1, len(content))
		out  []string
	)
	s.Init(file, content, nil, 0)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.IDENT {
			out = append(out, lit)
		}
	}
}

func identPrefixStart(content []byte, offset int) int {
	start := offset
	for start > 0 {
		r, size := utf8.DecodeLastRune(content[:start])
		if r == utf8.RuneError && size == 1 {
			break
		}
		if !isIdentifierChar(r) {
			break
		}
		start -= size
	}
	return start
}

func isIdentifierChar(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func (s *server) saveLastCompletion(uri protocol.DocumentURI, version int32, items []protocol.CompletionItem, pos protocol.Position) {
	s.efficacyMu.Lock()
	defer s.efficacyMu.Unlock()
	s.efficacyVersion = version
	s.efficacyURI = uri
	s.efficacyPos = pos
	s.efficacyItems = items
}

// toProtocolCompletionItems converts the candidates to the protocol completion items,
// the candidates must be sorted based on score as it will be respected by client side.
func toProtocolCompletionItems(candidates []completion.CompletionItem, surrounding *completion.Selection, options *settings.Options) ([]protocol.CompletionItem, error) {
	replaceRng, err := surrounding.Range()
	if err != nil {
		return nil, err
	}
	insertRng0, err := surrounding.PrefixRange()
	if err != nil {
		return nil, err
	}
	suffix := surrounding.Suffix()

	var (
		items                  = make([]protocol.CompletionItem, 0, len(candidates))
		numDeepCompletionsSeen int
	)
	for i, candidate := range candidates {
		// Limit the number of deep completions to not overwhelm the user in cases
		// with dozens of deep completion matches.
		if candidate.Depth > 0 {
			if !options.DeepCompletion {
				continue
			}
			if numDeepCompletionsSeen >= completion.MaxDeepCompletions {
				continue
			}
			numDeepCompletionsSeen++
		}
		insertText := candidate.InsertText
		if options.InsertTextFormat == protocol.SnippetTextFormat {
			insertText = candidate.Snippet()
		}

		// This can happen if the client has snippets disabled but the
		// candidate only supports snippet insertion.
		if insertText == "" {
			continue
		}

		var doc *protocol.Or_CompletionItem_documentation
		if candidate.Documentation != "" {
			var value any
			if options.PreferredContentFormat == protocol.Markdown {
				value = protocol.MarkupContent{
					Kind:  protocol.Markdown,
					Value: golang.DocCommentToMarkdown(candidate.Documentation, options),
				}
			} else {
				value = candidate.Documentation
			}
			doc = &protocol.Or_CompletionItem_documentation{Value: value}
		}
		var edits *protocol.Or_CompletionItem_textEdit
		if options.InsertReplaceSupported {
			insertRng := insertRng0
			if suffix == "" || strings.Contains(insertText, suffix) {
				insertRng = replaceRng
			}
			// Insert and Replace ranges share the same start position and
			// the same text edit but the end position may differ.
			// See the comment for the CompletionItem's TextEdit field.
			// https://pkg.go.dev/golang.org/x/tools/gopls/internal/protocol#CompletionItem
			edits = &protocol.Or_CompletionItem_textEdit{
				Value: protocol.InsertReplaceEdit{
					NewText: insertText,
					Insert:  insertRng, // replace up to the cursor position.
					Replace: replaceRng,
				},
			}
		} else {
			edits = &protocol.Or_CompletionItem_textEdit{
				Value: protocol.TextEdit{
					NewText: insertText,
					Range:   replaceRng,
				},
			}
		}
		item := protocol.CompletionItem{
			Label:               candidate.Label,
			Detail:              candidate.Detail,
			Kind:                candidate.Kind,
			TextEdit:            edits,
			InsertTextFormat:    &options.InsertTextFormat,
			AdditionalTextEdits: candidate.AdditionalTextEdits,
			// This is a hack so that the client sorts completion results in the order
			// according to their score. This can be removed upon the resolution of
			// https://github.com/Microsoft/language-server-protocol/issues/348.
			SortText: fmt.Sprintf("%05d", i),

			// Trim operators (VSCode doesn't like weird characters in
			// filterText).
			FilterText: strings.TrimLeft(candidate.InsertText, "&*"),

			Preselect:     i == 0,
			Documentation: doc,
			Tags:          protocol.NonNilSlice(candidate.Tags),
			Deprecated:    candidate.Deprecated,
		}
		items = append(items, item)
	}
	return items, nil
}
