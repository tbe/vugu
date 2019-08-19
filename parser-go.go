package vugu

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"path/filepath"
	"strings"

	"github.com/vugu/vugu/internal/htmlx"
	"github.com/vugu/vugu/internal/htmlx/atom"
)

// Parse2 is an experiment...
// r is the actual input, fname is only used to emit line directives
func (p *ParserGo) Parse(r io.Reader, fname string) error {

	state := &parseGoState{}

	inRaw, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	// use a tokenizer to peek at the first element and see if it's an HTML tag
	state.isFullHTML = false
	tmpZ := htmlx.NewTokenizer(bytes.NewReader(inRaw))
	for {
		tt := tmpZ.Next()
		if tt == htmlx.ErrorToken {
			return tmpZ.Err()
		}
		if tt != htmlx.StartTagToken { // skip over non-tags
			continue
		}
		t := tmpZ.Token()
		if t.Data == "html" {
			state.isFullHTML = true
			break
		}
		break
	}

	// log.Printf("isFullHTML: %v", state.isFullHTML)

	if state.isFullHTML {

		n, err := htmlx.Parse(bytes.NewReader(inRaw))
		if err != nil {
			return err
		}
		state.docNodeList = append(state.docNodeList, n) // docNodeList is just this one item

	} else {

		nlist, err := htmlx.ParseFragment(bytes.NewReader(inRaw), &htmlx.Node{
			Type:     htmlx.ElementNode,
			DataAtom: atom.Div,
			Data:     "div",
		})
		if err != nil {
			return err
		}

		// only add elements
		for _, n := range nlist {
			if n.Type != htmlx.ElementNode {
				continue
			}
			state.docNodeList = append(state.docNodeList, n)
		}

	}

	// run n through the optimizer and convert large chunks of static elements into
	// vg-html attributes, this should provide a significiant performance boost for static HTML
	if !p.NoOptimizeStatic {
		for _, n := range state.docNodeList {
			err = compactNodeTree(n)
			if err != nil {
				return err
			}
		}
	}

	// log.Printf("parsed document looks like so upon start of parsing:")
	// for i, n := range state.docNodeList {
	// 	var buf bytes.Buffer
	// 	err := htmlx.Render(&buf, n)
	// 	if err != nil {
	// 		return fmt.Errorf("error during debug render: %v", err)
	// 	}
	// 	log.Printf("state.docNodeList[%d]:\n%s", i, buf.Bytes())
	// }

	err = p.visitOverall(state)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	// log.Printf("goBuf.Len == %v", goBuf.Len())
	buf.Write(state.goBuf.Bytes())
	buf.Write(state.buildBuf.Bytes())
	buf.Write(state.goBufBottom.Bytes())

	outPath := filepath.Join(p.OutDir, p.OutFile)

	fo, err := p.gofmt(buf.String())
	if err != nil {

		// if the gofmt errors, we still attempt to write out the non-fmt'ed output to the file, to assist in debugging
		ioutil.WriteFile(outPath, buf.Bytes(), 0644)

		return err
	}

	err = ioutil.WriteFile(outPath, []byte(fo), 0644)
	if err != nil {
		return err
	}

	return nil
}

type codeChunk struct {
	Line   int
	Column int
	Code   string
}

type parseGoState struct {
	isFullHTML  bool          // is the first node an <html> tag
	docNodeList []*htmlx.Node // top level nodes parsed out of source file
	goBuf       bytes.Buffer  // additional Go code (at top)
	buildBuf    bytes.Buffer  // Build() method Go code (below)
	goBufBottom bytes.Buffer  // additional Go code that is put as the very last thing
	// cssChunkList []codeChunk
	// jsChunkList  []codeChunk
	outIsSet bool // set to true when vgout.Out has been set for to the level node
}

func (p *ParserGo) visitOverall(state *parseGoState) error {

	fmt.Fprintf(&state.goBuf, "package %s\n\n", p.PackageName)
	fmt.Fprintf(&state.goBuf, "// DO NOT EDIT: This file was generated by vugu. Please regenerate instead of editing or add additional code in a separate file.\n\n")
	fmt.Fprintf(&state.goBuf, "import %q\n", "fmt")
	fmt.Fprintf(&state.goBuf, "import %q\n", "reflect")
	fmt.Fprintf(&state.goBuf, "import %q\n", "github.com/vugu/vugu")
	fmt.Fprintf(&state.goBuf, "import js %q\n", "github.com/vugu/vugu/js")
	fmt.Fprintf(&state.goBuf, "\n")

	// TODO: we use a prefix like "vg" as our namespace; should document that user code should not use that prefix to avoid conflicts
	fmt.Fprintf(&state.buildBuf, "func (c *%s) Build(vgin *vugu.BuildIn) (vgout *vugu.BuildOut, vgreterr error) {\n", p.StructType)
	fmt.Fprintf(&state.buildBuf, "    \n")
	fmt.Fprintf(&state.buildBuf, "    vgout = &vugu.BuildOut{}\n")
	fmt.Fprintf(&state.buildBuf, "    \n")
	fmt.Fprintf(&state.buildBuf, "    var vgn *vugu.VGNode\n")
	// fmt.Fprintf(&buildBuf, "    var vgparent *vugu.VGNode\n")

	fmt.Fprintf(&state.goBufBottom, "// 'fix' unused imports\n")
	fmt.Fprintf(&state.goBufBottom, "var _ = fmt.Sprintf\n")
	fmt.Fprintf(&state.goBufBottom, "var _ = reflect.New\n")
	fmt.Fprintf(&state.goBufBottom, "var _ = js.ValueOf\n")
	fmt.Fprintf(&state.goBufBottom, "\n")

	// remove document node if present
	if len(state.docNodeList) == 1 && state.docNodeList[0].Type == htmlx.DocumentNode {
		state.docNodeList = []*htmlx.Node{state.docNodeList[0].FirstChild}
	}

	if state.isFullHTML {

		if len(state.docNodeList) != 1 {
			return fmt.Errorf("full HTML mode but not exactly 1 node found (found %d)", len(state.docNodeList))
		}
		err := p.visitHTML(state, state.docNodeList[0])
		if err != nil {
			return err
		}

	} else {

		for _, n := range state.docNodeList {

			// ignore comments
			if n.Type == htmlx.CommentNode {
				continue
			}

			if n.Type == htmlx.TextNode {

				// ignore whitespace text
				if strings.TrimSpace(n.Data) == "" {
					continue
				}

				// error on non-whitespace text
				return fmt.Errorf("unexpected text outside any element: %q", n.Data)

			}

			// must be an element at this point
			if n.Type != htmlx.ElementNode {
				return fmt.Errorf("unexpected node type %v; node=%#v", n.Type, n)
			}

			if isScriptOrStyle(n) {
				err := p.visitScriptOrStyle(state, n)
				if err != nil {
					return err
				}
				continue
			}

			// top node

			// check for forbidden top level tags
			nodeName := strings.ToLower(n.Data)
			if nodeName == "head" ||
				nodeName == "body" {
				return fmt.Errorf("component cannot use %q as top level tag", nodeName)
			}

			err := p.visitTopNode(state, n)
			if err != nil {
				return err
			}
			continue

		}

	}

	// for _, chunk := range state.cssChunkList {
	// 	// fmt.Fprintf(&buildBuf, "    out.AppendCSS(/*line %s:%d*/%q)\n\n", fname, chunk.Line, chunk.Code)
	// 	// fmt.Fprintf(&state.buildBuf, "    out.AppendCSS(%q)\n\n", chunk.Code)
	// 	_ = chunk
	// 	panic("need to append whole node, not AppendCSS")
	// }

	// for _, chunk := range state.jsChunkList {
	// 	// fmt.Fprintf(&buildBuf, "    out.AppendJS(/*line %s:%d*/%q)\n\n", fname, chunk.Line, chunk.Code)
	// 	// fmt.Fprintf(&state.buildBuf, "    out.AppendJS(%q)\n\n", chunk.Code)
	// 	_ = chunk
	// 	panic("need to append whole node, not AppendJS")
	// }

	fmt.Fprintf(&state.buildBuf, "    return vgout, nil\n")
	fmt.Fprintf(&state.buildBuf, "}\n\n")

	return nil
}

func (p *ParserGo) visitHTML(state *parseGoState, n *htmlx.Node) error {

	pOutputTag(state, n)
	// fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q,Attr:%#v}\n", n.Type, n.Data, staticVGAttrx(n.Attr))
	// fmt.Fprintf(&state.buildBuf, "vgout.Out = append(vgout.Out, vgn) // root for output\n") // for first element we need to assign as Doc on BuildOut
	// state.outIsSet = true

	// dynamic attrs
	dynExprMap, dynExprMapKeys := dynamicVGAttrExprx(n)
	for _, k := range dynExprMapKeys {
		valExpr := dynExprMap[k]
		fmt.Fprintf(&state.buildBuf, "vgn.Attr = append(vgn.Attr, vugu.VGAttribute{Key:%q,Val:fmt.Sprint(%s)})\n", k, valExpr)
	}

	fmt.Fprintf(&state.buildBuf, "{\n")
	fmt.Fprintf(&state.buildBuf, "vgparent := vgn; _ = vgparent\n") // vgparent set for this block to vgn

	for childN := n.FirstChild; childN != nil; childN = childN.NextSibling {

		if childN.Type != htmlx.ElementNode {
			continue
		}

		var err error
		if strings.ToLower(childN.Data) == "head" {
			err = p.visitHead(state, childN)
		} else if strings.ToLower(childN.Data) == "body" {
			err = p.visitBody(state, childN)
		} else {
			return fmt.Errorf("unknown tag inside html %q", childN.Data)

		}

		if err != nil {
			return err
		}

	}

	fmt.Fprintf(&state.buildBuf, "}\n")

	return nil
}

func (p *ParserGo) visitHead(state *parseGoState, n *htmlx.Node) error {

	pOutputTag(state, n)
	// fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q,Attr:%#v}\n", n.Type, n.Data, staticVGAttrx(n.Attr))
	// fmt.Fprintf(&state.buildBuf, "vgout.Out = append(vgout.Out, vgn) // root for output\n") // for first element we need to assign as Doc on BuildOut
	// state.outIsSet = true

	// dynamic attrs
	dynExprMap, dynExprMapKeys := dynamicVGAttrExprx(n)
	for _, k := range dynExprMapKeys {
		valExpr := dynExprMap[k]
		fmt.Fprintf(&state.buildBuf, "vgn.Attr = append(vgn.Attr, vugu.VGAttribute{Key:%q,Val:fmt.Sprint(%s)})\n", k, valExpr)
	}

	fmt.Fprintf(&state.buildBuf, "{\n")
	fmt.Fprintf(&state.buildBuf, "vgparent := vgn; _ = vgparent\n") // vgparent set for this block to vgn

	for childN := n.FirstChild; childN != nil; childN = childN.NextSibling {

		if isScriptOrStyle(childN) {
			err := p.visitScriptOrStyle(state, childN)
			if err != nil {
				return err
			}
			continue
		}

		err := p.visitDefaultByType(state, childN)
		if err != nil {
			return err
		}

	}

	fmt.Fprintf(&state.buildBuf, "}\n")

	return nil

}

func (p *ParserGo) visitBody(state *parseGoState, n *htmlx.Node) error {

	pOutputTag(state, n)
	// fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q,Attr:%#v}\n", n.Type, n.Data, staticVGAttrx(n.Attr))
	// fmt.Fprintf(&state.buildBuf, "vgout.Out = append(vgout.Out, vgn) // root for output\n") // for first element we need to assign as Doc on BuildOut
	// state.outIsSet = true

	// dynamic attrs
	dynExprMap, dynExprMapKeys := dynamicVGAttrExprx(n)
	for _, k := range dynExprMapKeys {
		valExpr := dynExprMap[k]
		fmt.Fprintf(&state.buildBuf, "vgn.Attr = append(vgn.Attr, vugu.VGAttribute{Key:%q,Val:fmt.Sprint(%s)})\n", k, valExpr)
	}

	fmt.Fprintf(&state.buildBuf, "{\n")
	fmt.Fprintf(&state.buildBuf, "vgparent := vgn; _ = vgparent\n") // vgparent set for this block to vgn

	foundMountEl := false

	for childN := n.FirstChild; childN != nil; childN = childN.NextSibling {

		// ignore whitespace and comments directly in body
		if childN.Type != htmlx.ElementNode {
			continue
		}

		if isScriptOrStyle(childN) {
			err := p.visitScriptOrStyle(state, childN)
			if err != nil {
				return err
			}
			continue
		}

		if foundMountEl {
			return fmt.Errorf("element %q found after we already have a mount element", childN.Data)
		}
		foundMountEl = true

		err := p.visitDefaultByType(state, childN)
		if err != nil {
			return err
		}

	}

	fmt.Fprintf(&state.buildBuf, "}\n")

	return nil

}

// visitScriptOrStyle calls visitJS, visitCSS or visitGo accordingly,
// will error if the node does not correspond to one of those
func (p *ParserGo) visitScriptOrStyle(state *parseGoState, n *htmlx.Node) error {

	nodeName := strings.ToLower(n.Data)

	// script tag
	if nodeName == "script" {

		var mt string // mime type

		ty := attrWithKey(n, "type")
		if ty == nil {
			//return fmt.Errorf("script tag without type attribute is not valid")
			mt = ""
		} else {
			mt, _, _ = mime.ParseMediaType(ty.Val)
		}

		// go code
		if mt == "application/x-go" {
			err := p.visitGo(state, n)
			if err != nil {
				return err
			}
			return nil
		}

		// component js (type attr omitted okay - means it is JS)
		if mt == "application/javascript" || mt == "" {
			err := p.visitJS(state, n)
			if err != nil {
				return err
			}
			return nil
		}

		return fmt.Errorf("found script tag with invalid mime type %q", mt)

	}

	// component css
	if nodeName == "style" || nodeName == "link" {
		err := p.visitCSS(state, n)
		if err != nil {
			return err
		}
		return nil
	}

	return fmt.Errorf("element %q is not a valid script or style - %#v", n.Data, n)
}

func (p *ParserGo) visitJS(state *parseGoState, n *htmlx.Node) error {

	if n.Type != htmlx.ElementNode {
		return fmt.Errorf("visitJS, not an element node %#v", n)
	}

	nodeName := strings.ToLower(n.Data)

	if nodeName != "script" {
		return fmt.Errorf("visitJS, tag %q not a script", nodeName)
	}

	// see if there's a script inside, or if this is a script include
	if n.FirstChild == nil {
		// script include - we pretty much just let this through, don't care what the attrs are
	} else {
		// if there is a script inside, we do not allow attributes other than "type", to avoid
		// people using features that might not be compatible with the funky stuff we have to do
		// in vugu to make all this work

		for _, a := range n.Attr {
			if a.Key != "type" {
				return fmt.Errorf("attribute %q not allowed on script tag that contains JS code", a.Key)
			}
			if a.Val != "application/javascript" {
				return fmt.Errorf("script type %q invalid (must be application/javascript)", a.Val)
			}
		}

		// verify that all children are text nodes
		for childN := n.FirstChild; childN != nil; childN = childN.NextSibling {
			if childN.Type != htmlx.ElementNode {
				return fmt.Errorf("style tag contains non-text child: %#v", childN)
			}
		}

	}

	// allow control stuff, why not

	// vg-for
	if forx := vgForExprx(n); forx != "" {
		// fmt.Fprintf(&buildBuf, "for /*line %s:%d*/%s {\n", fname, n.Line, forx)
		fmt.Fprintf(&state.buildBuf, "for %s {\n", forx)
		defer fmt.Fprintf(&state.buildBuf, "}\n")
	}

	// vg-if
	ife := vgIfExprx(n)
	if ife != "" {
		fmt.Fprintf(&state.buildBuf, "if %s {\n", ife)
		defer fmt.Fprintf(&state.buildBuf, "}\n")
	}

	// but then for the actual output, we append to vgout.JS, instead of parentNode
	fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q,Attr:%#v}\n", n.Type, n.Data, staticVGAttrx(n.Attr))
	fmt.Fprintf(&state.buildBuf, "vgout.JS = append(vgout.JS, vgn)\n")

	// dynamic attrs
	dynExprMap, dynExprMapKeys := dynamicVGAttrExprx(n)
	for _, k := range dynExprMapKeys {
		valExpr := dynExprMap[k]
		fmt.Fprintf(&state.buildBuf, "vgn.Attr = append(vgn.Attr, vugu.VGAttribute{Key:%q,Val:fmt.Sprint(%s)})\n", k, valExpr)
	}

	return nil
}

func (p *ParserGo) visitCSS(state *parseGoState, n *htmlx.Node) error {

	if n.Type != htmlx.ElementNode {
		return fmt.Errorf("visitCSS, not an element node %#v", n)
	}

	nodeName := strings.ToLower(n.Data)
	switch nodeName {
	case "link":

		// okay as long as nothing is inside this node

		if n.FirstChild != nil {
			return fmt.Errorf("link tag should not have children")
		}

	case "style":

		// okay as long as only text nodes inside
		for childN := n.FirstChild; childN != nil; childN = childN.NextSibling {
			if childN.Type != htmlx.ElementNode {
				return fmt.Errorf("style tag contains non-text child: %#v", childN)
			}
		}

	default:
		return fmt.Errorf("visitCSS, unexpected tag name %q", nodeName)
	}

	// allow control stuff, why not

	// vg-for
	if forx := vgForExprx(n); forx != "" {
		// fmt.Fprintf(&buildBuf, "for /*line %s:%d*/%s {\n", fname, n.Line, forx)
		fmt.Fprintf(&state.buildBuf, "for %s {\n", forx)
		defer fmt.Fprintf(&state.buildBuf, "}\n")
	}

	// vg-if
	ife := vgIfExprx(n)
	if ife != "" {
		fmt.Fprintf(&state.buildBuf, "if %s {\n", ife)
		defer fmt.Fprintf(&state.buildBuf, "}\n")
	}

	// but then for the actual output, we append to vgout.CSS, instead of parentNode
	fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q,Attr:%#v}\n", n.Type, n.Data, staticVGAttrx(n.Attr))
	fmt.Fprintf(&state.buildBuf, "vgout.CSS = append(vgout.CSS, vgn)\n")

	// dynamic attrs
	dynExprMap, dynExprMapKeys := dynamicVGAttrExprx(n)
	for _, k := range dynExprMapKeys {
		valExpr := dynExprMap[k]
		fmt.Fprintf(&state.buildBuf, "vgn.Attr = append(vgn.Attr, vugu.VGAttribute{Key:%q,Val:fmt.Sprint(%s)})\n", k, valExpr)
	}

	return nil
}

func (p *ParserGo) visitGo(state *parseGoState, n *htmlx.Node) error {

	for childN := n.FirstChild; childN != nil; childN = childN.NextSibling {
		if childN.Type != htmlx.TextNode {
			return fmt.Errorf("unexpected node type %v inside of script tag", childN.Type)
		}
		// if childN.Line > 0 {
		// 	fmt.Fprintf(&goBuf, "//line %s:%d\n", fname, childN.Line)
		// }
		state.goBuf.WriteString(childN.Data)
	}

	return nil
}

// visitTopNode handles the "mount point"
func (p *ParserGo) visitTopNode(state *parseGoState, n *htmlx.Node) error {

	// handle the top element other than <html>

	err := p.visitNodeJustElement(state, n)
	if err != nil {
		return err
	}

	return nil
}

// visitNodeElementAndCtrl handles an element that supports vg-if, vg-for etc
func (p *ParserGo) visitNodeElementAndCtrl(state *parseGoState, n *htmlx.Node) error {

	// vg-for
	if forx := vgForExprx(n); forx != "" {
		// fmt.Fprintf(&buildBuf, "for /*line %s:%d*/%s {\n", fname, n.Line, forx)
		fmt.Fprintf(&state.buildBuf, "for %s {\n", forx)
		defer fmt.Fprintf(&state.buildBuf, "}\n")
	}

	// vg-if
	ife := vgIfExprx(n)
	if ife != "" {
		fmt.Fprintf(&state.buildBuf, "if %s {\n", ife)
		defer fmt.Fprintf(&state.buildBuf, "}\n")
	}

	err := p.visitNodeJustElement(state, n)
	if err != nil {
		return err
	}

	return nil
}

// visitNodeJustElement handles an element, ignoring any vg-if, vg-for (but it does handle vg-html - since that is not really "control" just a shorthand for it's contents)
func (p *ParserGo) visitNodeJustElement(state *parseGoState, n *htmlx.Node) error {

	// regular element

	// if n.Line > 0 {
	// 	fmt.Fprintf(&buildBuf, "//line %s:%d\n", fname, n.Line)
	// }

	pOutputTag(state, n)
	// fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q,Attr:%#v}\n", n.Type, n.Data, staticVGAttrx(n.Attr))
	// if state.outIsSet {
	// 	fmt.Fprintf(&state.buildBuf, "vgparent.AppendChild(vgn)\n") // if not root, make AppendChild call
	// } else {
	// 	fmt.Fprintf(&state.buildBuf, "vgout.Out = append(vgout.Out, vgn) // root for output\n") // for first element we need to assign as Doc on BuildOut
	// 	state.outIsSet = true
	// }

	// dynamic attrs
	dynExprMap, dynExprMapKeys := dynamicVGAttrExprx(n)
	for _, k := range dynExprMapKeys {
		valExpr := dynExprMap[k]
		fmt.Fprintf(&state.buildBuf, "vgn.Attr = append(vgn.Attr, vugu.VGAttribute{Key:%q,Val:fmt.Sprint(%s)})\n", k, valExpr)
	}

	// vg-html
	htmlExpr := vgHTMLExprx(n)
	if htmlExpr != "" {
		fmt.Fprintf(&state.buildBuf, "{\nvghtml := %s; \nvgn.InnerHTML = &vghtml\n}\n", htmlExpr)
	}

	// DOM events
	eventMap, eventKeys := vgDOMEventExprsx(n)
	for _, k := range eventKeys {
		expr := eventMap[k]
		fmt.Fprintf(&state.buildBuf, "vgn.DOMEventHandlerSpecList = append(vgn.DOMEventHandlerSpecList, vugu.DOMEventHandlerSpec{\n")
		fmt.Fprintf(&state.buildBuf, "EventType: %q,\n", k)
		fmt.Fprintf(&state.buildBuf, "Func: func(event *vugu.DOMEvent) { %s },\n", expr)
		fmt.Fprintf(&state.buildBuf, "// TODO: implement capture, etc. mostly need to decide syntax\n")
		fmt.Fprintf(&state.buildBuf, "})\n")
	}

	if n.FirstChild != nil {

		fmt.Fprintf(&state.buildBuf, "{\n")
		fmt.Fprintf(&state.buildBuf, "vgparent := vgn; _ = vgparent\n") // vgparent set for this block to vgn

		// iterate over children
		for childN := n.FirstChild; childN != nil; childN = childN.NextSibling {

			err := p.visitDefaultByType(state, childN)
			if err != nil {
				return err
			}
		}

		fmt.Fprintf(&state.buildBuf, "}\n")

	}

	return nil
}

func (p *ParserGo) visitDefaultByType(state *parseGoState, n *htmlx.Node) error {

	// handle child according to type
	var err error
	switch {
	case n.Type == htmlx.CommentNode:
		err = p.visitNodeComment(state, n)
	case n.Type == htmlx.TextNode:
		err = p.visitNodeText(state, n)
	case n.Type == htmlx.ElementNode:
		if strings.Contains(n.Data, ":") {
			err = p.visitNodeComponentElement(state, n)
		} else {
			err = p.visitNodeElementAndCtrl(state, n)
		}
	default:
		return fmt.Errorf("child node of unknown type %v: %#v", n.Type, n)
	}

	if err != nil {
		return err
	}

	return nil
}

func (p *ParserGo) visitNodeText(state *parseGoState, n *htmlx.Node) error {

	fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q}\n", n.Type, n.Data)
	fmt.Fprintf(&state.buildBuf, "vgparent.AppendChild(vgn)\n")

	return nil
}

func (p *ParserGo) visitNodeComment(state *parseGoState, n *htmlx.Node) error {

	fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q}\n", n.Type, n.Data)
	fmt.Fprintf(&state.buildBuf, "vgparent.AppendChild(vgn)\n")

	return nil
}

// visitNodeComponentElement handles an element that is a call to a component
func (p *ParserGo) visitNodeComponentElement(state *parseGoState, n *htmlx.Node) error {

	nodeName := n.Data
	nodeNameParts := strings.Split(nodeName, ":")
	if len(nodeNameParts) != 2 {
		return fmt.Errorf("invalid component tag name %q must contain exactly one colon", nodeName)
	}

	// dynamic attrs

	// component events

	// slots

	return fmt.Errorf("component tag not yet supported (%q)", nodeName)
}

// isScriptOrStyle returns true if this is a "script", "style" or "link" tag
func isScriptOrStyle(n *htmlx.Node) bool {
	if n.Type != htmlx.ElementNode {
		return false
	}
	switch strings.ToLower(n.Data) {
	case "script", "style", "link":
		return true
	}
	return false
}

func pOutputTag(state *parseGoState, n *htmlx.Node) {

	fmt.Fprintf(&state.buildBuf, "vgn = &vugu.VGNode{Type:vugu.VGNodeType(%d),Data:%q,Attr:%#v}\n", n.Type, n.Data, staticVGAttrx(n.Attr))
	if state.outIsSet {
		fmt.Fprintf(&state.buildBuf, "vgparent.AppendChild(vgn)\n") // if not root, make AppendChild call
	} else {
		fmt.Fprintf(&state.buildBuf, "vgout.Out = append(vgout.Out, vgn) // root for output\n") // for first element we need to assign as Doc on BuildOut
		state.outIsSet = true
	}

}

func attrWithKey(n *htmlx.Node, key string) *htmlx.Attribute {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			return &n.Attr[i]
		}
	}
	return nil
}
