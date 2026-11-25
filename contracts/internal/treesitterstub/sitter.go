// Package sitter provides a zero-CGO stub for github.com/smacker/go-tree-sitter
// so nektar's pure-Go .runar.go covenant compiler and unit tests build without CGO.
// Parsing .runar.ts files is intentionally disabled in this pure-Go module.
package sitter

import (
	"context"
	"errors"
)

var errTSDisabled = errors.New("tree-sitter TypeScript parser disabled in pure-Go .runar.go build")

type Language struct{}
type Point struct {
	Row    uint32
	Column uint32
}
type Parser struct{}
type Tree struct{}
type Node struct{}
type TreeCursor struct{}

func NewParser() *Parser                              { return &Parser{} }
func (p *Parser) SetLanguage(*Language)               {}
func (p *Parser) Close()                              {}
func (p *Parser) ParseCtx(context.Context, *Tree, []byte) (*Tree, error) {
	return nil, errTSDisabled
}

func (t *Tree) Close()         {}
func (t *Tree) RootNode() *Node { return nil }

func (n *Node) Type() string                 { return "" }
func (n *Node) Content([]byte) string        { return "" }
func (n *Node) StartPoint() Point            { return Point{} }
func (n *Node) EndPoint() Point              { return Point{} }
func (n *Node) StartByte() uint32            { return 0 }
func (n *Node) EndByte() uint32              { return 0 }
func (n *Node) ChildCount() uint32           { return 0 }
func (n *Node) NamedChildCount() uint32      { return 0 }
func (n *Node) Child(int) *Node              { return nil }
func (n *Node) NamedChild(int) *Node         { return nil }
func (n *Node) ChildByFieldName(string) *Node { return nil }
func (n *Node) Parent() *Node                { return nil }
func (n *Node) PrevSibling() *Node           { return nil }
func (n *Node) NextSibling() *Node           { return nil }
func (n *Node) PrevNamedSibling() *Node      { return nil }
func (n *Node) NextNamedSibling() *Node      { return nil }
func (n *Node) IsNamed() bool                { return false }
func (n *Node) IsMissing() bool              { return false }
func (n *Node) HasError() bool               { return false }
func (n *Node) IsError() bool                { return false }

func NewTreeCursor(*Node) *TreeCursor    { return &TreeCursor{} }
func (c *TreeCursor) Close()             {}
func (c *TreeCursor) CurrentNode() *Node { return nil }
func (c *TreeCursor) CurrentFieldName() string { return "" }
func (c *TreeCursor) GoToFirstChild() bool { return false }
func (c *TreeCursor) GoToNextSibling() bool { return false }
func (c *TreeCursor) GoToParent() bool   { return false }
