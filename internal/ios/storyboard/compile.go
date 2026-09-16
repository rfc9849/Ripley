package storyboard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"howett.net/plist"
)

// CompiledLaunchStoryboard describes a storyboardc produced by ripley's UIKit
// storyboard encoder. The historical name is kept because launch storyboards
// were the first supported use; stock Flutter Main.storyboard files use the
// same two-NIB container shape.
type CompiledLaunchStoryboard struct {
	Name          string
	ControllerNib string
	ViewNib       string
}

type storyboardCompileOptions struct {
	requireLaunchScreen    bool
	allowedControllerClass string
}

// UnsupportedCompileError means the source is a valid UIKit Interface Builder
// document but contains launch UI that ripley's launch-only compiler does not yet
// encode. Callers may fall back to the UILaunchScreen lowering.
type UnsupportedCompileError struct {
	Reasons []string
}

func (e *UnsupportedCompileError) Error() string {
	if len(e.Reasons) == 0 {
		return "launch storyboard contains unsupported content"
	}
	return "launch storyboard contains unsupported content: " + strings.Join(e.Reasons, "; ")
}

func IsUnsupportedCompileError(err error) bool {
	var target *UnsupportedCompileError
	return errors.As(err, &target)
}

// CompileLaunchStoryboard compiles the static UIKit subset permitted in an iOS
// launch storyboard directly to a real .storyboardc directory. It does not run
// ibtool and is safe to use on Linux.
//
// The supported subset is intentionally launch-specific: UIViewController with
// static UIView/UIImageView/UILabel/bar descendants, colors, images, fonts,
// autoresizing masks, safe-area layout guides, size-class trait variations, and
// equality Auto Layout constraints. Runtime actions/outlets/segues/custom
// classes are rejected.
func CompileLaunchStoryboard(path, outputDir string) (*CompiledLaunchStoryboard, error) {
	doc, err := parseDocument(path)
	if err != nil {
		return nil, err
	}
	c := &launchNibCompiler{
		doc:      doc,
		path:     path,
		named:    namedColorDefinitions(doc),
		images:   launchImageSizes(doc),
		byID:     map[string]*nibObject{},
		viewNode: map[string]*node{},
	}
	result, sceneNib, viewNib, err := c.compile(storyboardCompileOptions{requireLaunchScreen: true})
	if err != nil {
		return nil, err
	}
	if len(c.unsupported) > 0 {
		sort.Strings(c.unsupported)
		return nil, &UnsupportedCompileError{Reasons: c.unsupported}
	}

	if err := writeStoryboardc(outputDir, result, sceneNib, viewNib); err != nil {
		return nil, err
	}
	return result, nil
}

func writeStoryboardc(outputDir string, result *CompiledLaunchStoryboard, sceneNib, viewNib []byte) error {
	if err := os.RemoveAll(outputDir); err != nil {
		return err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, result.ControllerNib+".nib"), sceneNib, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, result.ViewNib+".nib"), viewNib, 0o644); err != nil {
		return err
	}
	info := map[string]any{
		"UIStoryboardDesignatedEntryPointIdentifier": result.ControllerNib,
		"UIStoryboardVersion":                        uint64(1),
		"UIViewControllerIdentifiersToNibNames": map[string]any{
			result.ControllerNib: result.ControllerNib,
		},
	}
	data, err := plist.Marshal(info, plist.BinaryFormat)
	if err != nil {
		return fmt.Errorf("encode storyboard Info.plist: %w", err)
	}
	return os.WriteFile(filepath.Join(outputDir, "Info.plist"), data, 0o644)
}

// CompileMainStoryboard compiles the stock Flutter runtime main-interface
// storyboard. Unlike a launch screen, this storyboard must instantiate the
// real FlutterViewController before UIApplication calls the app delegate's
// didFinishLaunching method; many Flutter apps read AppDelegate.window there.
//
// Only the standard Flutter shape is accepted: one initial
// FlutterViewController with a plain root UIView and no runtime connections or
// additional content. Rich/custom main interfaces still require ibtool and are
// intentionally rejected rather than approximated.
func CompileMainStoryboard(path, outputDir string) (*CompiledLaunchStoryboard, error) {
	doc, err := parseDocument(path)
	if err != nil {
		return nil, err
	}
	analysis := analyzeMainInterface(doc, path)
	if !analysis.EquivalentToProgrammaticFlutterRoot {
		reasons := analysis.Reasons
		if len(reasons) == 0 {
			reasons = []string{"main interface is not the stock FlutterViewController storyboard"}
		}
		return nil, fmt.Errorf("unsupported main storyboard: %s", strings.Join(reasons, "; "))
	}
	c := &launchNibCompiler{
		doc:      doc,
		path:     path,
		named:    namedColorDefinitions(doc),
		images:   launchImageSizes(doc),
		byID:     map[string]*nibObject{},
		viewNode: map[string]*node{},
	}
	result, sceneNib, viewNib, err := c.compile(storyboardCompileOptions{allowedControllerClass: "FlutterViewController"})
	if err != nil {
		return nil, err
	}
	if len(c.unsupported) > 0 {
		sort.Strings(c.unsupported)
		return nil, fmt.Errorf("unsupported main storyboard: %s", strings.Join(c.unsupported, "; "))
	}
	if err := writeStoryboardc(outputDir, result, sceneNib, viewNib); err != nil {
		return nil, err
	}
	return result, nil
}

type launchNibCompiler struct {
	doc  *node
	path string

	named  map[string]Color
	images map[string][2]float64

	byID     map[string]*nibObject
	viewNode map[string]*node
	allView  []*nibObject

	traitStorages    []*nibObject
	traitDescendants []*nibObject
	defaultTraits    *nibObject

	unsupported []string
}

func (c *launchNibCompiler) compile(options storyboardCompileOptions) (*CompiledLaunchStoryboard, []byte, []byte, error) {
	if options.requireLaunchScreen && !c.doc.boolAttr("launchScreen") {
		c.note("document is not marked launchScreen=YES")
	}
	if c.doc.hasDescendant("segue", "action", "outlet", "outletCollection") {
		c.note("runtime storyboard connections/actions/segues are not valid static launch content")
	}
	scenes := documentScenes(c.doc)
	if len(scenes) == 0 {
		return nil, nil, nil, fmt.Errorf("%s: launch storyboard has no scene", c.path)
	}
	initial := c.doc.attr("initialViewController")
	main, _ := selectLaunchScene(scenes, initial)
	controllers := sceneControllers(main.objects)
	var controller *node
	for _, candidate := range controllers {
		if initial != "" && candidate.attr("id") == initial {
			controller = candidate
			break
		}
	}
	if controller == nil && len(controllers) > 0 {
		controller = controllers[0]
	}
	if controller == nil {
		return nil, nil, nil, fmt.Errorf("%s: launch scene has no view controller", c.path)
	}
	if controller.Name != "viewController" {
		c.note(fmt.Sprintf("initial controller <%s> is not a plain UIViewController", controller.Name))
	}
	controllerClass := "UIViewController"
	if class := controller.attr("customClass"); class != "" {
		if class == options.allowedControllerClass {
			controllerClass = class
		} else {
			c.note(fmt.Sprintf("custom view-controller class %q cannot be encoded in this storyboard mode", class))
		}
	}
	root := rootView(main.objects, controller)
	if root == nil {
		return nil, nil, nil, fmt.Errorf("%s: launch view controller has no root view", c.path)
	}
	if class := root.attr("customClass"); class != "" {
		c.note(fmt.Sprintf("custom root-view class %q cannot execute during static launch", class))
	}

	rootObj, err := c.buildView(root, true)
	if err != nil {
		return nil, nil, nil, err
	}
	// Legacy top/bottom view-controller guides are still present in many Flutter
	// template storyboards. Materialise them only when constraints reference them.
	if guides := controller.child("layoutGuides"); guides != nil {
		for _, g := range guides.Children {
			if g.Name != "viewControllerLayoutGuide" {
				continue
			}
			id := g.attr("id")
			if !c.documentReferencesID(id) {
				continue
			}
			guide := c.legacyLayoutGuide(g)
			c.byID[id] = guide
			c.allView = append(c.allView, guide)
		}
	}
	if err := c.attachConstraintsRecursive(root, rootObj); err != nil {
		return nil, nil, nil, err
	}

	controllerID := controller.attr("id")
	if controllerID == "" {
		return nil, nil, nil, fmt.Errorf("%s: initial view controller has no id", c.path)
	}
	viewID := root.attr("id")
	if viewID == "" {
		return nil, nil, nil, fmt.Errorf("%s: root view has no id", c.path)
	}
	controllerNib := controller.attr("storyboardIdentifier")
	if controllerNib == "" {
		controllerNib = "UIViewController-" + controllerID
	}
	viewNibName := controllerID + "-view-" + viewID

	viewRoot := c.viewNibRoot(rootObj)
	viewBytes, err := encodeNibArchive(viewRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode launch view nib: %w", err)
	}
	sceneRoot := c.sceneNibRoot(controllerNib, viewNibName, controllerClass)
	sceneBytes, err := encodeNibArchive(sceneRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode launch scene nib: %w", err)
	}
	return &CompiledLaunchStoryboard{
		Name:          DocumentName(c.path),
		ControllerNib: controllerNib,
		ViewNib:       viewNibName,
	}, sceneBytes, viewBytes, nil
}

func (c *launchNibCompiler) sceneNibRoot(controllerNib, viewNibName, className string) *nibObject {
	owner := nibProxy("IBFilesOwner")
	storyboard := nibProxy("UIStoryboardPlaceholder")
	if className == "" {
		className = "UIViewController"
	}
	controllerClass := nibString(className)
	originalClass := nibString("UIViewController")
	controller := nibObj("UIClassSwapper").
		add("UIExternalObjectsTableForViewLoading", nibEmptyDictionary()).
		add("UINibName", nibString(viewNibName)).
		add("UIClassName", controllerClass).
		add("UIOriginalClassName", originalClass)

	storyboardConnection := nibOutlet("storyboard", controller, storyboard)
	ownerConnection := nibOutlet("sceneViewController", owner, controller)
	empty := nibArray(nil, false)
	return nibObj("NSObject").
		add("UINibTopLevelObjectsKey", nibArray([]*nibObject{controller}, false)).
		add("UINibObjectsKey", nibArray([]*nibObject{controller}, false)).
		add("UINibConnectionsKey", nibArray([]*nibObject{storyboardConnection, ownerConnection}, false)).
		add("UINibVisibleWindowsKey", empty).
		add("UINibAccessibilityConfigurationsKey", empty).
		add("UINibTraitStorageListsKey", empty).
		add("UINibKeyValuePairsKey", empty).
		add("IBUINibLNEVersionKey", nibInt8(1))
}

func (c *launchNibCompiler) viewNibRoot(rootView *nibObject) *nibObject {
	owner := nibProxy("IBFilesOwner")
	connection := nibOutlet("view", owner, rootView)
	empty := nibArray(nil, false)
	traitLists := empty
	if len(c.traitStorages) > 0 {
		descendants := nibObj("NSSet")
		for i, object := range c.traitDescendants {
			descendants.add(fmt.Sprintf("NS.object.%d", i), object)
		}
		storageList := nibObj("_UITraitStorageList").
			add("UITopLevelObject", rootView).
			add("UITraitStorages", nibArray(c.traitStorages, false)).
			add("UIDescendants", descendants)
		traitLists = nibArray([]*nibObject{storageList}, false)
	}
	objects := make([]*nibObject, 0, len(c.allView))
	objects = append(objects, c.allView...)
	return nibObj("NSObject").
		add("UINibTopLevelObjectsKey", nibArray([]*nibObject{rootView}, false)).
		add("UINibObjectsKey", nibArray(objects, false)).
		add("UINibConnectionsKey", nibArray([]*nibObject{connection}, false)).
		add("UINibVisibleWindowsKey", empty).
		add("UINibAccessibilityConfigurationsKey", empty).
		add("UINibTraitStorageListsKey", traitLists).
		add("UINibKeyValuePairsKey", empty)
}

func nibProxy(identifier string) *nibObject {
	return nibObj("UIProxyObject").add("UIProxiedObjectIdentifier", nibString(identifier))
}

func nibOutlet(label string, source, destination *nibObject) *nibObject {
	return nibObj("UIRuntimeOutletConnection").
		add("UILabel", nibString(label)).
		add("UISource", source).
		add("UIDestination", destination)
}

var launchViewClasses = map[string]string{
	"view":          "UIView",
	"imageView":     "UIImageView",
	"label":         "UILabel",
	"navigationBar": "UINavigationBar",
	"tabBar":        "UITabBar",
	"toolbar":       "UIToolbar",
}

func (c *launchNibCompiler) buildView(n *node, root bool) (*nibObject, error) {
	c.validateVariations(n)
	class, ok := launchViewClasses[n.Name]
	if !ok {
		c.note(fmt.Sprintf("static <%s> view %s is not supported by the launch compiler", n.Name, n.attr("id")))
		class = "UIView"
	}
	if custom := n.attr("customClass"); custom != "" {
		c.note(fmt.Sprintf("custom view class %q on %s cannot execute during static launch", custom, n.attr("id")))
	}
	o := nibObj(class)
	id := n.attr("id")
	if id != "" {
		c.byID[id] = o
		c.viewNode[id] = n
	}
	c.allView = append(c.allView, o)

	x, y, w, h := frameForNode(n, root)
	o.add("UIBounds", nibStruct(0, 0, w, h)).add("UICenter", nibStruct(x+w/2, y+h/2))
	o.add("UIDeepDrawRect", false)
	if v := n.attr("opaque"); v != "" {
		o.add("UIOpaque", strings.EqualFold(v, "YES"))
	}
	o.add("UIAutoresizeSubviews", false)
	o.add("UIAutoresizingMask", nibInt8(autoresizingMask(n, root)))
	if mode := n.attr("contentMode"); mode != "" && mode != "scaleToFill" {
		if value, ok := contentModeValue(mode); ok {
			o.add("UIContentMode", nibInt8(value))
		} else {
			c.note(fmt.Sprintf("contentMode %q on %s is unsupported", mode, id))
		}
	}
	if n.attr("clipsSubviews") != "" {
		o.add("UIClipsToBounds", strings.EqualFold(n.attr("clipsSubviews"), "YES"))
	}
	if strings.EqualFold(n.attr("translatesAutoresizingMaskIntoConstraints"), "NO") {
		// This is the representation emitted by current ibtool for launch nibs.
		o.add("UIViewDoesNotTranslateAutoresizingMaskIntoConstraints", false)
	}
	o.add("UIViewSemanticContentAttribute", nibInt8(0)).add("UIViewLargeContentStoredProperties", nibNil{})

	var backgroundColor *nibObject
	if colorNode := n.childWithKey("color", "backgroundColor"); colorNode != nil {
		color, err := parseColor(colorNode, c.named)
		if err != nil {
			return nil, fmt.Errorf("%s background color: %w", id, err)
		}
		backgroundColor = nibUIColor(color)
		o.add("UIBackgroundColor", backgroundColor)
	}
	if backgroundColor != nil {
		if err := c.addBackgroundColorVariations(n, o, backgroundColor, root); err != nil {
			return nil, err
		}
	}

	switch n.Name {
	case "imageView":
		name := n.attr("image")
		if name == "" {
			if child := n.child("image"); child != nil {
				name = child.attr("name")
			}
		}
		if name != "" {
			image := c.imagePlaceholder(name)
			o.add("UIImage", image)
			c.addImageVariations(n, o, image, root)
		} else {
			c.note(fmt.Sprintf("image view %s has no image", id))
		}
	case "label":
		if text := labelText(n); text != "" {
			o.add("UIText", nibString(text))
		}
		if font := n.childWithKey("fontDescription", "fontDescription"); font != nil {
			o.add("UIFont", nibUIFont(font))
		}
		if textColor := n.childWithKey("color", "textColor"); textColor != nil {
			color, err := parseColor(textColor, c.named)
			if err != nil {
				return nil, fmt.Errorf("%s text color: %w", id, err)
			}
			o.add("UITextColor", nibUIColor(color))
		}
		if align, ok := textAlignmentValue(n.attr("textAlignment")); ok && align != 0 {
			o.add("UITextAlignment", nibInt8(align))
		}
	}

	// Safe-area guide belongs to the owning view and may be referenced by root
	// constraints. Build it before the constraints pass.
	var layoutGuides []*nibObject
	for _, child := range n.Children {
		if child.Name != "viewLayoutGuide" {
			continue
		}
		if child.attr("key") != "safeArea" {
			c.note(fmt.Sprintf("layout guide %s (%s) is not a safe-area guide", child.attr("id"), child.attr("key")))
			continue
		}
		guide := c.safeAreaGuide(child, o)
		c.byID[child.attr("id")] = guide
		c.allView = append(c.allView, guide)
		layoutGuides = append(layoutGuides, guide)
	}
	if len(layoutGuides) > 0 {
		o.add("UIViewLayoutGuides", nibArray(layoutGuides, true))
	}

	var subviews []*nibObject
	if container := n.child("subviews"); container != nil {
		for _, child := range container.Children {
			view, err := c.buildView(child, false)
			if err != nil {
				return nil, err
			}
			subviews = append(subviews, view)
		}
	}
	if len(subviews) > 0 {
		o.add("UISubviews", nibArray(subviews, true))
	}
	return o, nil
}

func (c *launchNibCompiler) validateVariations(n *node) {
	for _, variation := range parseTraitVariations(n) {
		recognized := false
		for name := range variation.node.Attr {
			switch name {
			case "key":
			case "image":
				if n.Name == "imageView" {
					recognized = true
				} else {
					c.note(fmt.Sprintf("trait variation %s on <%s> %s changes unsupported property %q", variation.label(), n.Name, n.attr("id"), name))
				}
			default:
				c.note(fmt.Sprintf("trait variation %s on <%s> %s changes unsupported property %q", variation.label(), n.Name, n.attr("id"), name))
			}
		}
		for _, child := range variation.node.Children {
			if child.Name == "color" && child.attr("key") == "backgroundColor" {
				recognized = true
				continue
			}
			c.note(fmt.Sprintf("trait variation %s on <%s> %s contains unsupported <%s> change", variation.label(), n.Name, n.attr("id"), child.Name))
		}
		if !recognized {
			c.note(fmt.Sprintf("trait variation %s on <%s> %s has no supported changed property", variation.label(), n.Name, n.attr("id")))
		}
	}
}

func (c *launchNibCompiler) imagePlaceholder(name string) *nibObject {
	// Asset-catalog images are resolved at runtime; current ibtool writes 1x1
	// dimensions for catalog placeholders, so use the same neutral size.
	return nibObj("UIImageNibPlaceholder").
		add("UIImageWidth", nibFloat(1)).
		add("UIImageHeight", nibFloat(1)).
		add("UIResourceName", nibString(name)).
		add("UISystemSymbolResourceName", nibNil{}).
		add("UISymbolImageConfiguration", nibNil{}).
		add("UIResourceCatalogName", nibNil{})
}

func (c *launchNibCompiler) addImageVariations(n *node, object, base *nibObject, root bool) {
	var records []*nibObject
	for _, variation := range parseTraitVariations(n) {
		name := variation.node.attr("image")
		if name == "" {
			continue
		}
		traits, ok := c.nibTraitCollection(variation)
		if !ok {
			continue
		}
		records = append(records, nibObj("_UIAttributeTraitStorageRecord").
			add("UITraitCollection", traits).
			add("UIValue", c.imagePlaceholder(name)))
	}
	if len(records) == 0 {
		return
	}
	c.addTraitStorage(object, "image", base, records, root)
}

func (c *launchNibCompiler) addBackgroundColorVariations(n *node, object, base *nibObject, root bool) error {
	var records []*nibObject
	for _, variation := range parseTraitVariations(n) {
		colorNode := variation.node.childWithKey("color", "backgroundColor")
		if colorNode == nil {
			continue
		}
		color, err := parseColor(colorNode, c.named)
		if err != nil {
			return fmt.Errorf("%s background color variation: %w", n.attr("id"), err)
		}
		traits, ok := c.nibTraitCollection(variation)
		if !ok {
			continue
		}
		records = append(records, nibObj("_UIAttributeTraitStorageRecord").
			add("UITraitCollection", traits).
			add("UIValue", nibUIColor(color)))
	}
	if len(records) > 0 {
		c.addTraitStorage(object, "backgroundColor", base, records, root)
	}
	return nil
}

func (c *launchNibCompiler) addTraitStorage(object *nibObject, keyPath string, base *nibObject, records []*nibObject, root bool) {
	if c.defaultTraits == nil {
		c.defaultTraits = nibObj("UITraitCollection")
	}
	records = append(records, nibObj("_UIAttributeTraitStorageRecord").
		add("UITraitCollection", c.defaultTraits).
		add("UIValue", base))
	storage := nibObj("_UIAttributeTraitStorage").
		add("UIObject", object).
		add("UIKeyPath", nibString(keyPath)).
		add("UIRecords", nibArray(records, true))
	c.traitStorages = append(c.traitStorages, storage)
	if !root {
		for _, existing := range c.traitDescendants {
			if existing == object {
				return
			}
		}
		c.traitDescendants = append(c.traitDescendants, object)
	}
}

func (c *launchNibCompiler) nibTraitCollection(variation traitVariation) (*nibObject, bool) {
	traits := nibObj("UITraitCollection")
	known := map[string]bool{"widthClass": true, "heightClass": true}
	var unknown []string
	for name := range variation.traits {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		c.note(fmt.Sprintf("trait variation %s contains unsupported trait %q", variation.label(), unknown[0]))
		return nil, false
	}
	for _, item := range []struct {
		name string
		key  string
	}{
		{"widthClass", "UITraitCollectionBuiltinTrait-_UITraitNameHorizontalSizeClass"},
		{"heightClass", "UITraitCollectionBuiltinTrait-_UITraitNameVerticalSizeClass"},
	} {
		value, exists := variation.traits[item.name]
		if !exists {
			continue
		}
		var encoded int8
		switch value {
		case "compact":
			encoded = 1
		case "regular":
			encoded = 2
		case "unspecified", "any":
			continue
		default:
			c.note(fmt.Sprintf("trait variation %s has unsupported %s value %q", variation.label(), item.name, value))
			return nil, false
		}
		traits.add(item.key, nibInt8(encoded))
	}
	return traits, true
}

func (c *launchNibCompiler) attachConstraintsRecursive(n *node, owner *nibObject) error {
	if constraints := n.child("constraints"); constraints != nil {
		var encoded []*nibObject
		for _, con := range constraints.children("constraint") {
			obj, err := c.buildConstraint(con, n.attr("id"), owner)
			if err != nil {
				return err
			}
			if obj != nil {
				encoded = append(encoded, obj)
				c.allView = append(c.allView, obj)
			}
		}
		if len(encoded) > 0 {
			owner.add("UIViewAutolayoutConstraints", nibArray(encoded, true))
		}
	}
	if subviews := n.child("subviews"); subviews != nil {
		for _, child := range subviews.Children {
			id := child.attr("id")
			obj := c.byID[id]
			if obj == nil {
				continue
			}
			if err := c.attachConstraintsRecursive(child, obj); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *launchNibCompiler) buildConstraint(n *node, ownerID string, owner *nibObject) (*nibObject, error) {
	if relation := n.attr("relation"); relation != "" && relation != "equal" {
		c.note(fmt.Sprintf("constraint %s relation %q is not yet supported", n.attr("id"), relation))
		return nil, nil
	}
	if multiplier := n.attr("multiplier"); multiplier != "" && multiplier != "1" && multiplier != "1:1" {
		c.note(fmt.Sprintf("constraint %s multiplier %q is not yet supported", n.attr("id"), multiplier))
		return nil, nil
	}
	firstID := n.attr("firstItem")
	if firstID == "" {
		firstID = ownerID
	}
	first := c.byID[firstID]
	if first == nil && firstID == ownerID {
		first = owner
	}
	if first == nil {
		c.note(fmt.Sprintf("constraint %s references unknown firstItem %q", n.attr("id"), firstID))
		return nil, nil
	}
	firstAttr, ok := layoutAttributeValue(n.attr("firstAttribute"))
	if !ok {
		c.note(fmt.Sprintf("constraint %s has unsupported firstAttribute %q", n.attr("id"), n.attr("firstAttribute")))
		return nil, nil
	}
	o := nibObj("NSLayoutConstraint").
		add("NSFirstItem", first).
		add("NSFirstAttributeV2", nibInt8(firstAttr)).
		add("NSFirstAttribute", nibInt8(firstAttr))

	if secondID := n.attr("secondItem"); secondID != "" {
		second := c.byID[secondID]
		if second == nil {
			c.note(fmt.Sprintf("constraint %s references unknown secondItem %q", n.attr("id"), secondID))
			return nil, nil
		}
		secondAttr, ok := layoutAttributeValue(n.attr("secondAttribute"))
		if !ok {
			c.note(fmt.Sprintf("constraint %s has unsupported secondAttribute %q", n.attr("id"), n.attr("secondAttribute")))
			return nil, nil
		}
		o.add("NSSecondItem", second).
			add("NSSecondAttributeV2", nibInt8(secondAttr)).
			add("NSSecondAttribute", nibInt8(secondAttr))
	}
	if constant := n.attr("constant"); constant != "" {
		v, err := strconv.ParseFloat(constant, 64)
		if err != nil {
			return nil, fmt.Errorf("constraint %s constant %q: %w", n.attr("id"), constant, err)
		}
		if v != 0 {
			o.add("NSConstantV2", nibDouble(v)).add("NSConstant", nibDouble(v))
		}
	}
	o.add("NSShouldBeArchived", false)
	if priority := n.attr("priority"); priority != "" && priority != "1000" {
		v, err := strconv.Atoi(priority)
		if err != nil || v < 0 || v > 32767 {
			c.note(fmt.Sprintf("constraint %s has unsupported priority %q", n.attr("id"), priority))
		} else {
			o.add("NSPriority", nibInt16(v))
		}
	}
	return o, nil
}

func (c *launchNibCompiler) safeAreaGuide(n *node, owner *nibObject) *nibObject {
	guide := nibObj("UILayoutGuide").
		add("UILayoutGuideOwningView", owner).
		add("UILayoutGuideIdentifier", nibString("UIViewSafeAreaLayoutGuide")).
		add("UILayoutGuideOwningViewIsLocked", false).
		add("UILayoutGuideShouldBeArchived", false).
		add("UILayoutGuideAllowsNegativeDimensions", false)
	return guide
}

func (c *launchNibCompiler) legacyLayoutGuide(n *node) *nibObject {
	identifier := ""
	switch n.attr("type") {
	case "top":
		identifier = "_UIViewControllerTop"
	case "bottom":
		identifier = "_UIViewControllerBottom"
	default:
		identifier = "_UIViewControllerLayoutGuide"
	}
	return nibObj("_UILayoutGuide").
		add("UIDeepDrawRect", false).
		add("UIOpaque", false).
		add("UIHidden", false).
		add("UIAutoresizeSubviews", false).
		add("UIViewDoesNotTranslateAutoresizingMaskIntoConstraints", false).
		add("UIViewSemanticContentAttribute", nibInt8(0)).
		add("UIViewLargeContentStoredProperties", nibNil{}).
		add("_UILayoutGuideIdentifier", nibString(identifier))
}

func (c *launchNibCompiler) documentReferencesID(id string) bool {
	if id == "" {
		return false
	}
	found := false
	c.doc.descendants(func(n *node) {
		if n.attr("firstItem") == id || n.attr("secondItem") == id {
			found = true
		}
	})
	return found
}

func (c *launchNibCompiler) note(reason string) {
	for _, existing := range c.unsupported {
		if existing == reason {
			return
		}
	}
	c.unsupported = append(c.unsupported, reason)
}

func frameForNode(n *node, root bool) (x, y, w, h float64) {
	for _, r := range n.children("rect") {
		if r.attr("key") != "frame" {
			continue
		}
		x, _ = strconv.ParseFloat(r.attr("x"), 64)
		y, _ = strconv.ParseFloat(r.attr("y"), 64)
		w, _ = strconv.ParseFloat(r.attr("width"), 64)
		h, _ = strconv.ParseFloat(r.attr("height"), 64)
		if w > 0 && h > 0 {
			return
		}
	}
	// ibtool uses a neutral 1000x1000 design-time frame when an old storyboard
	// omits explicit frames and relies entirely on Auto Layout.
	return 0, 0, 1000, 1000
}

func autoresizingMask(n *node, root bool) int8 {
	if mask := n.childWithKey("autoresizingMask", "autoresizingMask"); mask != nil {
		var out int8
		attrs := []string{"flexibleMinX", "widthSizable", "flexibleMaxX", "flexibleMinY", "heightSizable", "flexibleMaxY"}
		for i, attr := range attrs {
			if strings.EqualFold(mask.attr(attr), "YES") {
				out |= 1 << i
			}
		}
		return out
	}
	if root {
		return 18
	}
	return 36
}

func contentModeValue(mode string) (int8, bool) {
	modes := []string{"scaleToFill", "scaleAspectFit", "scaleAspectFill", "redraw", "center", "top", "bottom", "left", "right", "topLeft", "topRight", "bottomLeft", "bottomRight"}
	for i, candidate := range modes {
		if mode == candidate {
			return int8(i), true
		}
	}
	return 0, false
}

func textAlignmentValue(value string) (int8, bool) {
	if value == "" || value == "left" {
		return 0, true
	}
	values := map[string]int8{"center": 1, "right": 2, "justified": 3, "natural": 4}
	v, ok := values[value]
	return v, ok
}

func layoutAttributeValue(value string) (int8, bool) {
	attrs := map[string]int8{
		"left": 1, "right": 2, "top": 3, "bottom": 4,
		"leading": 5, "trailing": 6, "width": 7, "height": 8,
		"centerX": 9, "centerY": 10, "baseline": 11,
		"firstBaseline": 12, "leftMargin": 13, "rightMargin": 14,
		"topMargin": 15, "bottomMargin": 16, "leadingMargin": 17,
		"trailingMargin": 18, "centerXWithinMargins": 19,
		"centerYWithinMargins": 20, "lastBaseline": 11,
	}
	v, ok := attrs[value]
	return v, ok
}

func labelText(n *node) string {
	if text := n.attr("text"); text != "" {
		return text
	}
	for _, child := range n.children("string") {
		if child.attr("key") == "text" {
			return child.attr("value")
		}
	}
	return ""
}

func nibUIFont(n *node) *nibObject {
	size, _ := strconv.ParseFloat(n.attr("pointSize"), 64)
	if size <= 0 {
		size = 17
	}
	name := n.attr("name")
	traits := int8(0)
	system := false
	switch n.attr("type") {
	case "boldSystem":
		name, traits, system = ".HelveticaNeueInterface-MediumP4", 2, true
	case "italicSystem":
		name, traits, system = ".HelveticaNeueInterface-Italic", 1, true
	case "system", "":
		if name == "" {
			name = ".HelveticaNeueInterface-Regular"
		}
		system = true
	}
	if name == "" {
		name = ".HelveticaNeueInterface-Regular"
	}
	nameObj := nibString(name)
	return nibObj("UIFont").
		add("UIFontName", nameObj).
		add("UIFontPointSize", nibDouble(size)).
		add("UIFontTraits", nibInt8(traits)).
		add("UISystemFont", system).
		add("NSName", nameObj).
		add("NSSize", nibDouble(size))
}

func nibUIColor(c Color) *nibObject {
	o := nibObj("UIColor")
	if c.CatalogName != "" {
		o.add("UIDynamicCatalogName", nibString(c.CatalogName)).
			add("UIDynamicCatalogUseNibBundle", false)
	}
	if c.SystemName != "" {
		o.add("UISystemColorName", nibString(c.SystemName))
	}
	if c.SystemName != "" && nearlyEqual(c.R, c.G) && nearlyEqual(c.G, c.B) {
		o.add("UIColorComponentCount", nibInt8(2)).
			add("UIWhite", nibFloat(c.R)).
			add("UIAlpha", nibFloat(c.A)).
			add("NSWhite", nibData([]byte(compactFloat(c.R)))).
			add("NSColorSpace", nibInt8(4))
		return o
	}
	o.add("UIColorComponentCount", nibInt8(4)).
		add("UIRed", nibFloat(c.R)).
		add("UIGreen", nibFloat(c.G)).
		add("UIBlue", nibFloat(c.B)).
		add("UIAlpha", nibFloat(c.A)).
		add("NSRGB", nibData([]byte(compactFloat(c.R)+" "+compactFloat(c.G)+" "+compactFloat(c.B)))).
		add("NSColorSpace", nibInt8(2))
	return o
}

func compactFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func nearlyEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func launchImageSizes(doc *node) map[string][2]float64 {
	out := map[string][2]float64{}
	resources := doc.child("resources")
	if resources == nil {
		return out
	}
	for _, img := range resources.children("image") {
		name := img.attr("name")
		if name == "" {
			continue
		}
		w, _ := strconv.ParseFloat(img.attr("width"), 64)
		h, _ := strconv.ParseFloat(img.attr("height"), 64)
		out[name] = [2]float64{w, h}
	}
	return out
}
