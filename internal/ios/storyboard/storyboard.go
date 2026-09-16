// Package storyboard implements the launch-screen part of Interface Builder on
// Linux, where Apple's `ibtool` is unavailable.
//
// CompileLaunchStoryboard turns the static UIKit subset allowed in a launch
// storyboard into a real `.storyboardc`: binary NIBArchive coder-v10 scene/view
// archives plus the storyboard Info.plist. The compiler is pure Go and has no
// Xcode/macOS runtime dependency. Unsupported launch-only constructs are
// reported explicitly so callers can fall back to the iOS 14+ `UILaunchScreen`
// lowering produced by ParseLaunchScreen instead of silently dropping UI.
//
// ParseLaunchScreen therefore remains both the compatibility fallback and the
// asset-dependency analyser. Every lossy fallback decision is recorded in
// LaunchScreen.Unsupported.
//
// This package MUST NOT import ripley/internal/ios; the dependency runs the
// other way.
package storyboard

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strings"
)

// node is a generic Interface Builder XML element. IB documents are
// attribute-heavy and reference objects by id across sibling subtrees, and the
// set of element names is open-ended (every UIKit view class has one), so a
// generic tree keeps unknown content visible for reporting instead of silently
// discarding it.
type node struct {
	Name     string
	Attr     map[string]string
	Children []*node
}

func (n *node) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	n.Name = start.Name.Local
	if len(start.Attr) > 0 {
		n.Attr = make(map[string]string, len(start.Attr))
		for _, a := range start.Attr {
			n.Attr[a.Name.Local] = a.Value
		}
	}
	for {
		tok, err := d.Token()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			child := &node{}
			if err := child.UnmarshalXML(d, t); err != nil {
				return err
			}
			n.Children = append(n.Children, child)
		case xml.EndElement:
			return nil
		}
	}
}

func (n *node) attr(name string) string {
	if n == nil {
		return ""
	}
	return n.Attr[name]
}

func (n *node) boolAttr(name string) bool {
	return strings.EqualFold(n.attr(name), "YES")
}

// child returns the first direct child with the given element name.
func (n *node) child(name string) *node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// childWithKey returns the first direct child with the given element name whose
// `key` attribute matches. IB uses `key` for keyed relationships such as
// <view key="view"> and <color key="backgroundColor">.
func (n *node) childWithKey(name, key string) *node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name && c.attr("key") == key {
			return c
		}
	}
	return nil
}

// children returns all direct children with the given element name.
func (n *node) children(name string) []*node {
	if n == nil {
		return nil
	}
	var out []*node
	for _, c := range n.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// descendants walks the whole subtree, including n, in document order.
func (n *node) descendants(visit func(*node)) {
	if n == nil {
		return
	}
	visit(n)
	for _, c := range n.Children {
		c.descendants(visit)
	}
}

// hasDescendant reports whether any element in the subtree has one of the given
// names.
func (n *node) hasDescendant(names ...string) bool {
	found := false
	n.descendants(func(c *node) {
		for _, name := range names {
			if c.Name == name {
				found = true
			}
		}
	})
	return found
}

const (
	docTypeStoryboard = "com.apple.InterfaceBuilder3.CocoaTouch.Storyboard.XIB"
	docTypeXIB        = "com.apple.InterfaceBuilder3.CocoaTouch.XIB"
)

// parseDocument reads an Interface Builder document and verifies it targets
// UIKit. AppKit documents (macOS MainMenu.xib) have no iOS launch semantics at
// all, so they are rejected rather than mis-lowered.
func parseDocument(path string) (*node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseDocumentBytes(data, path)
}

func parseDocumentBytes(data []byte, path string) (*node, error) {
	doc := &node{}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	// IB documents are UTF-8 and self-contained; the decoder must not try to
	// resolve external entities or DTDs.
	dec.Strict = true
	if err := dec.Decode(doc); err != nil {
		return nil, fmt.Errorf("%s: parse Interface Builder document: %w", path, err)
	}
	if doc.Name != "document" {
		return nil, fmt.Errorf("%s: not an Interface Builder document (root element <%s>)", path, doc.Name)
	}
	switch t := doc.attr("type"); t {
	case docTypeStoryboard, docTypeXIB:
	default:
		return nil, fmt.Errorf("%s: not a UIKit Interface Builder document (type %q)", path, t)
	}
	return doc, nil
}

// scenePayload is one scene's object graph, plus its scene id for diagnostics.
type scenePayload struct {
	id      string
	objects *node
}

// scenes returns the object graphs of a document. Storyboards wrap objects in
// <scenes><scene><objects>; a standalone .xib has a single top-level <objects>.
func documentScenes(doc *node) []scenePayload {
	var out []scenePayload
	if scenes := doc.child("scenes"); scenes != nil {
		for _, s := range scenes.children("scene") {
			if objs := s.child("objects"); objs != nil {
				out = append(out, scenePayload{id: s.attr("sceneID"), objects: objs})
			}
		}
		return out
	}
	if objs := doc.child("objects"); objs != nil {
		out = append(out, scenePayload{objects: objs})
	}
	return out
}

// controllerElements are the scene-level objects that own a root view.
var controllerElements = map[string]bool{
	"viewController":           true,
	"tableViewController":      true,
	"collectionViewController": true,
	"navigationController":     true,
	"tabBarController":         true,
	"splitViewController":      true,
	"pageViewController":       true,
}

// sceneControllers returns the view-controller-ish objects of a scene.
func sceneControllers(objects *node) []*node {
	var out []*node
	for _, c := range objects.Children {
		if controllerElements[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// rootView locates the view a controller presents, or the top-level view of a
// standalone .xib.
func rootView(objects, controller *node) *node {
	if controller != nil {
		if v := controller.childWithKey("view", "view"); v != nil {
			return v
		}
		// Table/collection controllers name their root view differently.
		for _, c := range controller.Children {
			if strings.HasSuffix(strings.ToLower(c.Name), "view") && c.attr("key") == "view" {
				return c
			}
		}
		return nil
	}
	for _, c := range objects.Children {
		if c.Name == "view" && c.attr("key") == "" {
			return c
		}
	}
	return nil
}
