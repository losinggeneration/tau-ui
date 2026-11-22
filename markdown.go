package main

import (
	"bytes"
	"html/template"
	"log"
	"net/http"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

var codeFormat = chromahtml.New(chromahtml.WithClasses(true))
var codeStyle = styles.Get("github-dark")
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(highlightCode{}, 100))),
)

type highlightCode struct{}

func (highlightCode) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, renderFence)
}

func renderFence(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	block := node.(*ast.FencedCodeBlock)
	lexer := lexers.Get(string(block.Language(source)))
	if lexer == nil {
		lexer = lexers.Fallback
	}
	var code bytes.Buffer
	for i := range block.Lines().Len() {
		line := block.Lines().At(i)
		code.Write(line.Value(source))
	}
	iter, err := chroma.Coalesce(lexer).Tokenise(nil, code.String())
	if err == nil {
		err = codeFormat.Format(w, codeStyle, iter)
	}
	return ast.WalkSkipChildren, err
}

func renderMarkdown(text string) template.HTML {
	var out bytes.Buffer
	// Goldmark's unsafe option stays off. The code formatter escapes tokens.
	if err := markdown.Convert([]byte(text), &out); err != nil {
		log.Printf("render Markdown: %v", err)
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(out.String())
}

func codeCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	if err := codeFormat.WriteCSS(w, codeStyle); err != nil {
		log.Printf("write code styles: %v", err)
	}
}
