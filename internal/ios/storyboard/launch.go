package storyboard

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// RequiredColor is a color the lowered UILaunchScreen resolves by name through
// UIColor(named:), so the asset catalog must contain it as a `.colorset`.
//
// UILaunchScreen's UIColorName can only name an asset catalog color, so a
// literal <color> in a storyboard has to be materialised as a generated
// colorset. Name is the colorset name referenced by InfoPlistKeys; R, G, B and A
// are non-premultiplied sRGB components in 0..1, matching assetcatalog.Color.
//
// Dynamic reports a color that differs between light and dark appearance;
// DarkR..DarkA then hold the dark-appearance components and the colorset needs a
// dark "luminosity" appearance entry. For a non-dynamic color the dark
// components equal the light ones.
//
// Generated distinguishes the two cases, and consumers MUST honour it:
//
//   - Generated == true: the storyboard used a literal or system color that has
//     no catalog entry, so ripley must synthesize this colorset.
//   - Generated == false: the storyboard referenced a colorset the project
//     already authors (<color name="LaunchBackground"/>). The catalog's own
//     definition is authoritative and MUST NOT be overwritten. The components
//     here come from the document's <namedColor> mirror, which Interface Builder
//     writes for its canvas preview and which carries only the light
//     appearance — harmonoid's real LaunchBackground.colorset, for instance, also
//     defines a dark appearance that the mirror omits. Use these components only
//     as a fallback when the catalog genuinely lacks the colorset.
type RequiredColor struct {
	Name       string
	R, G, B, A float64

	Dynamic                    bool
	DarkR, DarkG, DarkB, DarkA float64

	Generated bool
}

// LaunchScreen is the lowering of one Interface Builder launch document.
type LaunchScreen struct {
	// Path is the source document.
	Path string

	// InfoPlistKeys holds the Info.plist keys to merge into the app's
	// Info.plist. It contains "UILaunchScreen" (a map[string]any), and
	// additionally "UILaunchScreens" when the document has idiom-specific
	// content. It is never nil: a launch document that contributes nothing but
	// a background still yields an empty UILaunchScreen dictionary, which is
	// what opts UIKit out of the legacy letterboxed launch mode.
	InfoPlistKeys map[string]any

	// RequiredImages lists asset names the bundle must be able to resolve via
	// UIImage(named:). They come from the document's <resources><image> entries
	// for images actually referenced by the lowered keys.
	RequiredImages []string

	// RequiredColors lists colorsets the asset catalog must contain, including
	// ones ripley generates for literal storyboard colors.
	RequiredColors []RequiredColor

	// Unsupported names launch UI this lowering cannot reproduce, each entry a
	// specific reason. InfoPlistKeys is still correct for what is supported.
	Unsupported []string

	// Background is the resolved root-view background color, if the document had
	// one.
	Background    *Color
	hasBackground bool
}

// imageView is a lowered <imageView> from a launch document.
type imageView struct {
	id          string
	image       string
	contentMode string
	// fullscreen is true when constraints pin the view to all four edges of the
	// root view.
	fullscreen bool
	// centered is true when constraints center the view in the root view.
	centered bool
	// hasFixedSize is true when constraints give the view an explicit
	// width/height.
	hasFixedSize bool
}

// ParseLaunchScreen parses an Interface Builder launch screen document and
// lowers it to iOS 14+ UILaunchScreen Info.plist semantics.
//
// The returned LaunchScreen always carries usable InfoPlistKeys; content that
// cannot be expressed as a UILaunchScreen dictionary is reported in Unsupported
// rather than dropped or approximated silently. An error is returned only when
// the file is not a readable UIKit Interface Builder document.
func ParseLaunchScreen(path string) (*LaunchScreen, error) {
	doc, err := parseDocument(path)
	if err != nil {
		return nil, err
	}
	return lowerLaunchScreen(doc, path)
}

func lowerLaunchScreen(doc *node, path string) (*LaunchScreen, error) {
	ls := &LaunchScreen{Path: path, InfoPlistKeys: map[string]any{}}

	named := namedColorDefinitions(doc)
	declaredImages := documentImageResources(doc)

	scenes := documentScenes(doc)
	if len(scenes) == 0 {
		ls.unsupported("document has no scenes; nothing to lower")
		ls.InfoPlistKeys["UILaunchScreen"] = map[string]any{}
		return ls, nil
	}

	// A launch storyboard must have exactly one scene. Extra scenes cannot be
	// reached during launch and IB itself only ever instantiates the initial
	// view controller, but report them so a hand-edited document is not
	// silently truncated.
	initial := doc.attr("initialViewController")
	main, others := selectLaunchScene(scenes, initial)
	for _, s := range others {
		ls.unsupported(fmt.Sprintf("scene %s is not the initial view controller and is ignored; a plist launch screen has a single scene", sceneLabel(s)))
	}
	if main.objects == nil {
		ls.unsupported("initial scene has no objects")
		ls.InfoPlistKeys["UILaunchScreen"] = map[string]any{}
		return ls, nil
	}

	controllers := sceneControllers(main.objects)
	var controller *node
	if initial != "" {
		for _, c := range controllers {
			if c.attr("id") == initial {
				controller = c
				break
			}
		}
	}
	if controller == nil && len(controllers) > 0 {
		controller = controllers[0]
	}
	if controller != nil {
		if class := controller.attr("customClass"); class != "" {
			ls.unsupported(fmt.Sprintf("initial view controller has custom class %q; its code cannot run before launch, only its static view content is lowered", class))
		}
		if controller.Name != "viewController" {
			ls.unsupported(fmt.Sprintf("initial controller is <%s>, not a plain view controller; container chrome is not reproduced", controller.Name))
		}
	}

	view := rootView(main.objects, controller)
	if view == nil {
		ls.unsupported("initial view controller has no root view")
		ls.InfoPlistKeys["UILaunchScreen"] = map[string]any{}
		return ls, nil
	}

	if class := view.attr("customClass"); class != "" {
		ls.unsupported(fmt.Sprintf("root view has custom class %q; only its background and image content are lowered", class))
	}

	// Background color.
	launch := map[string]any{}
	if bg := view.childWithKey("color", "backgroundColor"); bg != nil {
		c, err := parseColor(bg, named)
		if err != nil {
			ls.unsupported(fmt.Sprintf("root view background color not understood: %v", err))
		} else {
			ls.Background = &c
			ls.hasBackground = true
			name := c.colorsetName()
			launch["UIColorName"] = name
			ls.requireColor(c, name)
		}
	}

	// Subviews.
	subviews := view.child("subviews")
	var images []imageView
	if subviews != nil {
		constraints := parseConstraints(view, view.attr("id"))
		for _, sv := range subviews.Children {
			switch sv.Name {
			case "imageView":
				iv := imageView{
					id:          sv.attr("id"),
					image:       sv.attr("image"),
					contentMode: sv.attr("contentMode"),
				}
				if sv.attr("image") == "" {
					if imgChild := sv.child("image"); imgChild != nil {
						iv.image = imgChild.attr("name")
					}
				}
				c := constraints[iv.id]
				iv.fullscreen = c.pinsAllEdges()
				iv.centered = c.centersBoth()
				iv.hasFixedSize = c.hasWidth && c.hasHeight
				if iv.image == "" {
					ls.unsupported(fmt.Sprintf("image view %s has no image; an empty image view has no plist equivalent", iv.id))
					continue
				}
				images = append(images, iv)
			case "navigationBar", "tabBar", "toolbar":
				// Bars are the one kind of chrome UILaunchScreen does render.
				// UIBarDictionary carries only a background image, so the bar
				// itself lowers exactly while any buttons/titles inside it do
				// not.
				key := map[string]string{
					"navigationBar": "UINavigationBar",
					"tabBar":        "UITabBar",
					"toolbar":       "UIToolbar",
				}[sv.Name]
				bar := map[string]any{}
				if img := sv.attr("backgroundImage"); img != "" {
					bar["UIImageName"] = img
					ls.requireImage(img, declaredImages)
				}
				launch[key] = bar
				if items := sv.child("items"); items != nil && len(items.Children) > 0 {
					ls.unsupported(fmt.Sprintf("<%s> %s has %d item(s); %s renders an empty bar because UILaunchScreen cannot describe bar contents", sv.Name, sv.attr("id"), len(items.Children), key))
				}
			case "label":
				text := sv.attr("text")
				if text == "" {
					if s := sv.child("string"); s != nil {
						text = s.attr("key")
					}
				}
				ls.unsupported(fmt.Sprintf("label %s (text %q) cannot be reproduced: UILaunchScreen has no text element; render the text into the launch image instead", sv.attr("id"), text))
			case "view":
				ls.unsupported(fmt.Sprintf("plain subview %s (a colored container) cannot be reproduced: UILaunchScreen has no nested views", sv.attr("id")))
			default:
				ls.unsupported(fmt.Sprintf("subview <%s> %s cannot be reproduced: UILaunchScreen supports only a background color, one image and empty bars", sv.Name, sv.attr("id")))
			}
		}
	}

	// Image selection. UILaunchScreen renders exactly one image, centered, at
	// its natural size, so keep the image the storyboard renders that way and
	// report the rest.
	chosen, extra := selectLaunchImage(images)
	chosenID := ""
	if chosen != nil {
		chosenID = chosen.id
		launch["UIImageName"] = chosen.image
		ls.requireImage(chosen.image, declaredImages)
		// UIImageRespectsSafeAreaInsets defaults to false, meaning the image
		// covers the full screen. A storyboard image pinned to the root view's
		// edges (not its safe area) is the full-bleed case; an image pinned to
		// the safe area layout guide respects the insets.
		if imageRespectsSafeArea(view, chosen.id) {
			launch["UIImageRespectsSafeAreaInsets"] = true
		}
		ls.noteImageModeFidelity(chosen)
	}
	for _, iv := range extra {
		ls.unsupported(fmt.Sprintf("image view %s (image %q, contentMode %q) is dropped: UILaunchScreen renders a single UIImageName", iv.id, iv.image, iv.contentMode))
	}

	ls.InfoPlistKeys["UILaunchScreen"] = launch

	// Idiom-specific variants from <variation> trait overrides.
	if variants := idiomVariants(view, launch, chosenID, named, ls.unsupported); variants != nil {
		ls.InfoPlistKeys["UILaunchScreens"] = variants.screens
		if variants.ipadOnly != nil {
			ls.InfoPlistKeys["UILaunchScreen~ipad"] = variants.ipadOnly
		}
		for _, name := range variants.images {
			ls.requireImage(name, declaredImages)
		}
		for _, rc := range variants.colors {
			ls.requireColor(rc.color, rc.name)
		}
	}

	sort.Strings(ls.RequiredImages)
	return ls, nil
}

// noteImageModeFidelity records the visual difference between the storyboard's
// content mode and UILaunchScreen's fixed rendering.
//
// UIKit draws UIImageName centered at the image's natural size (the asset's
// own scale), and lets the background color show through. That matches
// contentMode="center" exactly. Any scaling mode is a real visual difference and
// is reported.
func (ls *LaunchScreen) noteImageModeFidelity(iv *imageView) {
	switch iv.contentMode {
	case "", "center":
		// Exact match.
	case "scaleAspectFit", "scaleAspectFill", "scaleToFill":
		if iv.hasFixedSize {
			ls.unsupported(fmt.Sprintf("image %q uses contentMode %q with an explicit size; UILaunchScreen draws the asset centered at its natural size, so the image will not be resized (provide a pre-sized asset to match)", iv.image, iv.contentMode))
		} else {
			ls.unsupported(fmt.Sprintf("image %q uses contentMode %q; UILaunchScreen draws the asset centered at its natural size and cannot scale it", iv.image, iv.contentMode))
		}
	default:
		ls.unsupported(fmt.Sprintf("image %q uses contentMode %q, which UILaunchScreen cannot express; the asset is drawn centered at its natural size", iv.image, iv.contentMode))
	}
}

// selectLaunchImage picks the image UILaunchScreen should render, and returns
// the ones it cannot.
//
// UILaunchScreen draws exactly one image, centered, at its natural size. So the
// image to keep is the one the storyboard also draws centered at natural size.
// Two independent things express that, and real templates use both:
//
//   - constraints that center the view in its parent (Flutter's stock template,
//     harmonoid, saber, smooth_app);
//   - contentMode="center", which draws the asset at natural size in the middle
//     of the view regardless of how the view itself is stretched. spotube and
//     immich pin the logo view to all four edges *and* set contentMode="center",
//     so the rendered result is a centered natural-size logo even though the
//     view is full-bleed.
//
// Ranking by "is drawn centered at natural size" therefore picks the logo, not
// the full-bleed background that happens to be listed first. A full-bleed
// scaling image is only chosen when nothing in the document is centered, since
// then it is the sole launch art.
func selectLaunchImage(images []imageView) (*imageView, []imageView) {
	if len(images) == 0 {
		return nil, nil
	}
	pick := 0
	best := -1
	for i := range images {
		if r := images[i].centeringRank(); r > best {
			best, pick = r, i
		}
	}
	var extra []imageView
	for i := range images {
		if i != pick {
			extra = append(extra, images[i])
		}
	}
	return &images[pick], extra
}

// centeringRank scores how closely an image view matches UILaunchScreen's fixed
// rendering (centered, natural size). Higher is closer.
func (iv imageView) centeringRank() int {
	naturalSize := iv.contentMode == "center"
	switch {
	case iv.centered && naturalSize:
		// Centered by constraints and drawn at natural size: exactly what
		// UILaunchScreen does.
		return 4
	case naturalSize:
		// Drawn at natural size in the middle of its view; the view's own
		// stretching does not move the artwork.
		return 3
	case iv.centered:
		// Centered, but scaled to a constrained size.
		return 2
	case iv.fullscreen:
		// Full-bleed scaling art.
		return 1
	default:
		return 0
	}
}

// constraintInfo summarises the constraints that target one subview.
type constraintInfo struct {
	top, bottom, leading, trailing bool
	centerX, centerY               bool
	hasWidth, hasHeight            bool
	// safeAreaEdges is true when the pinned edges reference the safe area
	// layout guide rather than the root view.
	safeAreaEdges bool
}

func (c constraintInfo) pinsAllEdges() bool {
	return c.top && c.bottom && c.leading && c.trailing
}

func (c constraintInfo) centersBoth() bool { return c.centerX && c.centerY }

// parseConstraints indexes a root view's <constraints> by the subview they
// affect.
//
// Interface Builder writes a constraint as a first item/attribute pair plus an
// optional second item/attribute, and omits firstItem when the first item is the
// view that owns the constraints. Both orderings appear in real Flutter
// templates (`firstItem="image" firstAttribute="leading" secondItem="root"` and
// `firstAttribute="trailing" secondItem="image"`), so both are folded onto the
// subview.
func parseConstraints(view *node, rootID string) map[string]constraintInfo {
	out := map[string]constraintInfo{}
	container := view.child("constraints")
	if container == nil {
		return out
	}
	safeAreaIDs := map[string]bool{}
	view.descendants(func(n *node) {
		if n.Name == "viewLayoutGuide" && n.attr("key") == "safeArea" {
			safeAreaIDs[n.attr("id")] = true
		}
	})

	// Edge attributes are mirrored: pinning a subview's trailing edge to the
	// root is the same relation as pinning the root's trailing edge to the
	// subview.
	mark := func(id, attribute string, againstSafeArea bool) {
		if id == "" || id == rootID {
			return
		}
		c := out[id]
		switch attribute {
		case "top":
			c.top = true
		case "bottom":
			c.bottom = true
		case "leading", "left":
			c.leading = true
		case "trailing", "right":
			c.trailing = true
		case "centerX":
			c.centerX = true
		case "centerY":
			c.centerY = true
		case "width":
			c.hasWidth = true
		case "height":
			c.hasHeight = true
		}
		if againstSafeArea {
			c.safeAreaEdges = true
		}
		out[id] = c
	}

	for _, con := range container.children("constraint") {
		first, firstAttr := con.attr("firstItem"), con.attr("firstAttribute")
		second, secondAttr := con.attr("secondItem"), con.attr("secondAttribute")
		if first == "" {
			first = rootID
		}
		if second == "" && secondAttr != "" {
			second = rootID
		}

		// Size constraints have no second item.
		if second == "" {
			mark(first, firstAttr, false)
			continue
		}

		firstIsSafeArea := safeAreaIDs[first]
		secondIsSafeArea := safeAreaIDs[second]
		// A constraint between a subview and the root (or its safe area)
		// contributes to that subview's layout on both sides.
		if !firstIsSafeArea {
			mark(first, firstAttr, secondIsSafeArea)
		}
		if !secondIsSafeArea {
			mark(second, secondAttr, firstIsSafeArea)
		}
	}
	return out
}

// imageRespectsSafeArea reports whether the chosen image view is laid out
// against the safe area rather than the full root view.
func imageRespectsSafeArea(view *node, id string) bool {
	c := parseConstraints(view, view.attr("id"))[id]
	return c.safeAreaEdges
}

// launchVariants is the idiom-specific lowering of a launch document.
//
// images and colors are the extra asset dependencies introduced by the iPad
// variant, so the caller can add them to RequiredImages/RequiredColors without
// re-walking the document.
type launchVariants struct {
	screens  map[string]any
	ipadOnly map[string]any
	images   []string
	colors   []namedRequiredColor
}

// namedRequiredColor pairs a generated colorset name with its resolved color.
type namedRequiredColor struct {
	name  string
	color Color
}

// traitVariation is one <variation> override attached to an element.
//
// Interface Builder expresses per-trait content with a <variation> child whose
// `key` names a size-class combination, e.g.
//
//	<imageView image="Logo" id="x">
//	    <variation key="heightClass=regular-widthClass=regular" image="LogoPad"/>
//	</imageView>
//
// The pairs are extracted by name so the parse does not depend on their order
// or on the separator Interface Builder chose.
type traitVariation struct {
	traits map[string]string
	node   *node
}

var traitPairRE = regexp.MustCompile(`([A-Za-z]+)=([A-Za-z]+)`)

func parseTraitVariations(n *node) []traitVariation {
	var out []traitVariation
	for _, v := range n.children("variation") {
		key := v.attr("key")
		if key == "" {
			continue
		}
		traits := map[string]string{}
		for _, m := range traitPairRE.FindAllStringSubmatch(key, -1) {
			traits[m[1]] = m[2]
		}
		if len(traits) == 0 {
			continue
		}
		out = append(out, traitVariation{traits: traits, node: v})
	}
	return out
}

// isIPadOnly reports whether a size-class combination is reachable only on iPad.
//
// Among the iOS idioms only iPad is regular in both dimensions: an iPhone is
// compact-width in portrait and compact-height in landscape, including the
// largest models. A regular/regular variation is therefore exactly the iPad
// idiom and can be lowered to the `~ipad` Info.plist suffix. Any other
// combination describes an orientation or a size class that spans idioms, which
// no Info.plist launch key can express.
func (t traitVariation) isIPadOnly() bool {
	return t.traits["widthClass"] == "regular" && t.traits["heightClass"] == "regular"
}

func (t traitVariation) label() string {
	names := make([]string, 0, len(t.traits))
	for k := range t.traits {
		names = append(names, k+"="+t.traits[k])
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// idiomVariants builds UILaunchScreens/UILaunchScreen~ipad when a document
// carries iPad-specific content.
//
// Only a regular/regular trait variation maps onto a device idiom, so only that
// one is lowered; every other variation is reported through report so the caller
// can list it as an unreproducible difference. When nothing distinguishes iPad
// from iPhone there is one launch description and a single UILaunchScreen
// dictionary is both correct and simpler, so nil is returned.
func idiomVariants(view *node, base map[string]any, chosenImageID string, named map[string]Color, report func(string)) *launchVariants {
	ipad := map[string]any{}
	for k, v := range base {
		ipad[k] = v
	}
	varied := false
	var extraImages []string
	var extraColors []namedRequiredColor

	// Root-view background variation.
	for _, tv := range parseTraitVariations(view) {
		if !tv.isIPadOnly() {
			report(fmt.Sprintf("root view has a trait variation (%s) that no Info.plist launch key can express; the base appearance is used for every size class", tv.label()))
			continue
		}
		if bg := tv.node.childWithKey("color", "backgroundColor"); bg != nil {
			c, err := parseColor(bg, named)
			if err != nil {
				report(fmt.Sprintf("iPad background color variation not understood: %v", err))
				continue
			}
			name := c.colorsetName()
			ipad["UIColorName"] = name
			extraColors = append(extraColors, namedRequiredColor{name: name, color: c})
			varied = true
		}
	}

	// Image variation on the image view that was actually chosen.
	if subviews := view.child("subviews"); subviews != nil {
		for _, sv := range subviews.Children {
			for _, tv := range parseTraitVariations(sv) {
				if !tv.isIPadOnly() {
					report(fmt.Sprintf("subview %s has a trait variation (%s) that no Info.plist launch key can express; the base appearance is used for every size class", sv.attr("id"), tv.label()))
					continue
				}
				img := tv.node.attr("image")
				if img == "" {
					continue
				}
				if sv.attr("id") != chosenImageID {
					report(fmt.Sprintf("iPad image variation %q on subview %s is dropped: it belongs to an image view that UILaunchScreen does not render", img, sv.attr("id")))
					continue
				}
				ipad["UIImageName"] = img
				extraImages = append(extraImages, img)
				varied = true
			}
		}
	}

	if !varied {
		return nil
	}

	phone := map[string]any{}
	for k, v := range base {
		phone[k] = v
	}

	const (
		defaultID = "FLLaunchScreenDefault"
		ipadID    = "FLLaunchScreenIPad"
	)
	phone["UILaunchScreenIdentifier"] = defaultID
	ipadDef := map[string]any{}
	for k, v := range ipad {
		ipadDef[k] = v
	}
	ipadDef["UILaunchScreenIdentifier"] = ipadID

	return &launchVariants{
		screens: map[string]any{
			"UILaunchScreenDefinitions":       []any{phone, ipadDef},
			"UIDefaultLaunchScreen":           defaultID,
			"UIURLToLaunchScreenAssociations": map[string]any{},
		},
		ipadOnly: ipad,
		images:   extraImages,
		colors:   extraColors,
	}
}

// documentImageResources returns the image names a document declares in its
// <resources> section, mapped to their declared point size when present.
func documentImageResources(doc *node) map[string]string {
	res := doc.child("resources")
	if res == nil {
		return nil
	}
	out := map[string]string{}
	for _, img := range res.children("image") {
		name := img.attr("name")
		if name == "" {
			continue
		}
		w, h := img.attr("width"), img.attr("height")
		if w != "" && h != "" {
			out[name] = w + "x" + h
		} else {
			out[name] = ""
		}
	}
	return out
}

func (ls *LaunchScreen) requireImage(name string, declared map[string]string) {
	for _, existing := range ls.RequiredImages {
		if existing == name {
			return
		}
	}
	ls.RequiredImages = append(ls.RequiredImages, name)
	if _, ok := declared[name]; !ok && declared != nil {
		ls.unsupported(fmt.Sprintf("image %q is referenced but not declared in the document's <resources>; it must exist in the asset catalog", name))
	}
}

// requireColor records the colorset a lowered UIColorName depends on.
//
// A color the storyboard referenced by catalog name (CatalogName set) already
// exists in the project's asset catalog and MUST NOT be overwritten: the real
// catalog may define appearances the storyboard's <namedColor> mirror omits.
// harmonoid is exactly this case — its LaunchBackground.colorset carries a dark
// appearance that the storyboard does not mention. Only a literal or system
// color is ripley's to generate.
func (ls *LaunchScreen) requireColor(c Color, name string) {
	for _, existing := range ls.RequiredColors {
		if existing.Name == name {
			return
		}
	}
	rc := RequiredColor{
		Name:      name,
		R:         c.R,
		G:         c.G,
		B:         c.B,
		A:         c.A,
		Dynamic:   c.Dynamic,
		Generated: c.CatalogName == "",
	}
	if c.Dynamic {
		rc.DarkR, rc.DarkG, rc.DarkB, rc.DarkA = c.DarkR, c.DarkG, c.DarkB, c.DarkA
	} else {
		rc.DarkR, rc.DarkG, rc.DarkB, rc.DarkA = c.R, c.G, c.B, c.A
	}
	ls.RequiredColors = append(ls.RequiredColors, rc)
}

func (ls *LaunchScreen) unsupported(reason string) {
	for _, existing := range ls.Unsupported {
		if existing == reason {
			return
		}
	}
	ls.Unsupported = append(ls.Unsupported, reason)
}

// selectLaunchScene picks the scene holding the initial view controller.
func selectLaunchScene(scenes []scenePayload, initial string) (scenePayload, []scenePayload) {
	if initial != "" {
		for i, s := range scenes {
			for _, c := range sceneControllers(s.objects) {
				if c.attr("id") == initial {
					var others []scenePayload
					others = append(others, scenes[:i]...)
					others = append(others, scenes[i+1:]...)
					return s, others
				}
			}
		}
	}
	return scenes[0], scenes[1:]
}

func sceneLabel(s scenePayload) string {
	if s.id != "" {
		return s.id
	}
	return "(unnamed)"
}

func sortedAttrNames(n *node) []string {
	out := make([]string, 0, len(n.Attr))
	for k := range n.Attr {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DocumentName returns the storyboard name a project's UIMainStoryboardFile /
// UILaunchStoryboardName value would refer to for this document (the base name
// without extension).
func DocumentName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}
