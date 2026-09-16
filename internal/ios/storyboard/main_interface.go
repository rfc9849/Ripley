package storyboard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MainInterface is the analysis of a project's main-interface storyboard: the
// document named by UIMainStoryboardFile (or a scene's
// UISceneStoryboardFile/UIWindowSceneSessionRoleApplication entry).
//
// A main interface storyboard is fundamentally different from a launch screen.
// It is not static art: it instantiates a view controller class at runtime and
// UIKit reads it from a compiled nib, so there is no Info.plist form that can
// stand in for it. This type therefore reports *why* the storyboard cannot be
// lowered, as data rather than as an error, so the build can keep the
// programmatic SceneDelegate substitution as its documented behaviour and
// explain the decision.
type MainInterface struct {
	// Path is the source document.
	Path string

	// Name is the value a project's UIMainStoryboardFile would carry for this
	// document.
	Name string

	// Lowerable is true only when the storyboard is pure static content that a
	// UILaunchScreen dictionary can express. For a real main interface it is
	// false: the storyboard's job is to instantiate a controller class.
	Lowerable bool

	// Reasons lists the specific properties that prevent lowering, in document
	// order. Empty when Lowerable is true.
	Reasons []string

	// RootControllerClass is the custom class of the initial view controller,
	// empty when the storyboard uses a plain UIViewController. For a Flutter
	// Runner this is "FlutterViewController", which is exactly what the
	// programmatic SceneDelegate substitution creates, so the substitution is
	// faithful.
	RootControllerClass string

	// EquivalentToProgrammaticFlutterRoot is true when the storyboard does
	// nothing but present a FlutterViewController with a plain background: the
	// programmatic SceneDelegate reproduces it exactly, with no visual
	// difference. This is the case for every stock Flutter Runner Main.storyboard.
	EquivalentToProgrammaticFlutterRoot bool

	// Background is the initial view controller's root view background color
	// when the document sets one. The programmatic substitution should apply it
	// to its window so the first frame matches.
	Background *Color
}

// flutterRootControllerClasses are the controller classes whose behaviour the
// programmatic SceneDelegate substitution reproduces exactly.
var flutterRootControllerClasses = map[string]bool{
	"FlutterViewController": true,
}

// AnalyzeMainInterface reads a main-interface storyboard and reports whether it
// can be lowered to Info.plist keys.
//
// It never returns an error for unsupported content; an error means the file
// could not be read or is not a UIKit Interface Builder document. Callers use
// the result to decide between honouring the storyboard and substituting a
// programmatic root, and to explain that choice.
func AnalyzeMainInterface(path string) (*MainInterface, error) {
	doc, err := parseDocument(path)
	if err != nil {
		return nil, err
	}
	return analyzeMainInterface(doc, path), nil
}

func analyzeMainInterface(doc *node, path string) *MainInterface {
	mi := &MainInterface{Path: path, Name: DocumentName(path)}

	named := namedColorDefinitions(doc)
	scenes := documentScenes(doc)
	initial := doc.attr("initialViewController")
	main, others := selectLaunchScene(scenes, initial)
	for _, s := range others {
		mi.Reasons = append(mi.Reasons, fmt.Sprintf("scene %s is an additional scene; Info.plist launch keys describe a single screen", sceneLabel(s)))
	}
	if main.objects == nil {
		mi.Reasons = append(mi.Reasons, "document has no scene objects")
		return mi
	}

	controllers := sceneControllers(main.objects)
	var controller *node
	for _, c := range controllers {
		if initial != "" && c.attr("id") == initial {
			controller = c
			break
		}
	}
	if controller == nil && len(controllers) > 0 {
		controller = controllers[0]
	}
	if controller == nil {
		mi.Reasons = append(mi.Reasons, "document has no view controller")
		return mi
	}

	mi.RootControllerClass = controller.attr("customClass")
	view := rootView(main.objects, controller)
	if view != nil {
		if bg := view.childWithKey("color", "backgroundColor"); bg != nil {
			if c, err := parseColor(bg, named); err == nil {
				mi.Background = &c
			}
		}
	}

	// Any real view content means the storyboard draws something the
	// programmatic root does not.
	var contentElements []string
	if view != nil {
		if subviews := view.child("subviews"); subviews != nil {
			for _, sv := range subviews.Children {
				contentElements = append(contentElements, fmt.Sprintf("<%s> %s", sv.Name, sv.attr("id")))
			}
		}
	}

	// Segues and connections are runtime behaviour with no plist form.
	var connections int
	doc.descendants(func(n *node) {
		switch n.Name {
		case "segue", "outlet", "action", "outletCollection":
			connections++
		}
	})

	if mi.RootControllerClass != "" {
		mi.Reasons = append(mi.Reasons, fmt.Sprintf("initial view controller instantiates custom class %q at runtime; Info.plist launch keys cannot instantiate a class", mi.RootControllerClass))
	} else {
		mi.Reasons = append(mi.Reasons, "a main interface storyboard defines the app's first real view controller, which Info.plist launch keys cannot instantiate")
	}
	for _, e := range contentElements {
		mi.Reasons = append(mi.Reasons, fmt.Sprintf("root view contains %s, which the programmatic root does not draw", e))
	}
	if connections > 0 {
		mi.Reasons = append(mi.Reasons, fmt.Sprintf("document has %d storyboard connection(s)/segue(s), which require a compiled nib", connections))
	}

	mi.EquivalentToProgrammaticFlutterRoot = flutterRootControllerClasses[mi.RootControllerClass] &&
		len(contentElements) == 0 && connections == 0 && len(others) == 0
	// A main interface storyboard always instantiates a controller class, so
	// Reasons is never empty and Lowerable is never true. Deriving it here keeps
	// the two fields consistent by construction rather than by assumption.
	mi.Lowerable = len(mi.Reasons) == 0

	return mi
}

// LaunchStoryboardPath locates the launch storyboard a project's
// UILaunchStoryboardName refers to, searching the localisation directories Xcode
// uses. It returns "" when the project has no such document, which is the
// zulip-flutter case: a Runner with Main.storyboard and no LaunchScreen.
func LaunchStoryboardPath(resourceDir, name string) string {
	if name == "" {
		return ""
	}
	name = strings.TrimSuffix(name, ".storyboard")
	candidates := []string{
		filepath.Join(resourceDir, "Base.lproj", name+".storyboard"),
		filepath.Join(resourceDir, name+".storyboard"),
		filepath.Join(resourceDir, "en.lproj", name+".storyboard"),
		filepath.Join(resourceDir, "Base.lproj", name+".xib"),
		filepath.Join(resourceDir, name+".xib"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}
