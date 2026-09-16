package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Xcode 16 (project object version 77) replaces per-file PBXFileReference lists
// with folder-synchronized groups: PBXFileSystemSynchronizedRootGroup names a
// directory on disk and every file below it is a member of every target that
// lists the group in fileSystemSynchronizedGroups. Deviations from that default
// live in PBXFileSystemSynchronizedBuildFileExceptionSet objects.
const (
	isaSynchronizedRootGroup    = "PBXFileSystemSynchronizedRootGroup"
	isaSynchronizedGroup        = "PBXFileSystemSynchronizedGroup"
	isaSynchronizedExceptionSet = "PBXFileSystemSynchronizedBuildFileExceptionSet"
)

// synchronizedException is one PBXFileSystemSynchronizedBuildFileExceptionSet.
//
// A membershipExceptions list means "exclude" when the referenced target owns
// the group (the group appears in that target's fileSystemSynchronizedGroups)
// and "include" when it does not: that is how Xcode keeps Info.plist out of the
// target owning the folder (immich's WidgetExtension) and how it feeds a single
// file from an unowned folder into a target (immich's Runner/Utility/Mutex.swift,
// where Utility is not in Runner's fileSystemSynchronizedGroups).
type synchronizedException struct {
	TargetID       string
	ExcludedFiles  []string
	IncludedFiles  []string
	PublicHeaders  []string
	PrivateHeaders []string
}

// synchronizedGroup is one PBXFileSystemSynchronizedRootGroup. Path is the raw
// pbxproj path value; it is resolved against the group's parent chain and
// sourceTree by targetSynchronizedSources.
type synchronizedGroup struct {
	ID              string
	Path            string
	SourceTree      string
	ExplicitFolders []string
	Exceptions      []synchronizedException
}

// opaqueSynchronizedDirs are directory extensions Xcode synchronizes as a single
// member instead of recursing into them.
var opaqueSynchronizedDirs = map[string]bool{
	".xcassets":       true,
	".xcstickers":     true,
	".bundle":         true,
	".framework":      true,
	".xcframework":    true,
	".docc":           true,
	".scnassets":      true,
	".xcdatamodeld":   true,
	".xcmappingmodel": true,
	".playground":     true,
	".rcproject":      true,
	".lproj":          false,
}

var pbxObjectVersionRE = regexp.MustCompile(`(?m)^\s*(?:preferredProjectObjectVersion|objectVersion)\s*=\s*"?([0-9]+)"?\s*;`)

// projectObjectVersion returns the highest of objectVersion and
// preferredProjectObjectVersion found in a project.pbxproj. Xcode 16 keeps a
// backwards-compatible objectVersion (54 in immich and spotube) while recording
// the format it really wants in preferredProjectObjectVersion (77), so the
// maximum is the value that tells a caller whether modern features may appear.
// Returns 0 when neither key is present.
func projectObjectVersion(pbx string) int {
	best := 0
	for _, match := range pbxObjectVersionRE.FindAllStringSubmatch(pbx, -1) {
		value, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if value > best {
			best = value
		}
	}
	return best
}

// parseSynchronizedGroups returns every PBXFileSystemSynchronizedRootGroup in
// the project keyed by object ID, with each exception set already classified
// into excluded and included membership. A project with no synchronized groups
// yields an empty map and no error.
func parseSynchronizedGroups(objects map[string]pbxObject) (map[string]synchronizedGroup, error) {
	owners := synchronizedGroupOwners(objects)
	groups := make(map[string]synchronizedGroup)
	for _, object := range objects {
		fields := pbxEntries(object.Body)
		if pbxEntryValue(fields, "isa") != isaSynchronizedRootGroup {
			continue
		}
		group := synchronizedGroup{
			ID:              object.ID,
			Path:            pbxEntryValue(fields, "path"),
			SourceTree:      pbxEntryValue(fields, "sourceTree"),
			ExplicitFolders: normalizeSynchronizedPaths(pbxEntryList(fields, "explicitFolders")),
		}
		if group.Path == "" {
			return nil, fmt.Errorf("%s %s has no path", isaSynchronizedRootGroup, object.ID)
		}
		for _, exceptionID := range pbxEntryIDs(fields, "exceptions") {
			exceptionObject, ok := objects[exceptionID]
			if !ok {
				return nil, fmt.Errorf("%s %s references missing exception set %s", isaSynchronizedRootGroup, object.ID, exceptionID)
			}
			exceptionFields := pbxEntries(exceptionObject.Body)
			if isa := pbxEntryValue(exceptionFields, "isa"); isa != isaSynchronizedExceptionSet {
				return nil, fmt.Errorf("%s %s exception %s has isa %q, want %s", isaSynchronizedRootGroup, object.ID, exceptionID, isa, isaSynchronizedExceptionSet)
			}
			targetID := pbxEntryID(exceptionFields, "target")
			if targetID == "" {
				return nil, fmt.Errorf("%s %s has no target", isaSynchronizedExceptionSet, exceptionID)
			}
			exception := synchronizedException{
				TargetID:       targetID,
				PublicHeaders:  normalizeSynchronizedPaths(pbxEntryList(exceptionFields, "publicHeaders")),
				PrivateHeaders: normalizeSynchronizedPaths(pbxEntryList(exceptionFields, "privateHeaders")),
			}
			membership := normalizeSynchronizedPaths(pbxEntryList(exceptionFields, "membershipExceptions"))
			if owners[object.ID][targetID] {
				exception.ExcludedFiles = membership
			} else {
				exception.IncludedFiles = membership
			}
			group.Exceptions = append(group.Exceptions, exception)
		}
		sort.SliceStable(group.Exceptions, func(i, j int) bool {
			return group.Exceptions[i].TargetID < group.Exceptions[j].TargetID
		})
		groups[object.ID] = group
	}
	return groups, nil
}

// targetSynchronizedSources returns the absolute paths of every file on disk
// that belongs to target through folder-synchronized groups. srcRoot is SRCROOT
// (the directory holding the .xcodeproj). Members are returned unclassified,
// deduplicated and sorted by path; callers filter by extension. A target with no
// synchronized groups yields nil and no error.
func targetSynchronizedSources(objects map[string]pbxObject, target xcodeTarget, srcRoot string) ([]string, error) {
	groups, err := parseSynchronizedGroups(objects)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	absRoot, err := filepath.Abs(srcRoot)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]bool)
	if targetObject, ok := objects[target.ID]; ok {
		for _, id := range pbxEntryIDs(pbxEntries(targetObject.Body), "fileSystemSynchronizedGroups") {
			owned[id] = true
		}
	}
	parents := pbxGroupParents(objects)

	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	members := make(map[string]bool)
	for _, id := range ids {
		group := groups[id]
		exception := group.exceptionFor(target.ID)
		if !owned[id] && len(exception.IncludedFiles) == 0 {
			continue
		}
		dir, err := resolveSynchronizedGroupDir(objects, parents, id, absRoot)
		if err != nil {
			return nil, err
		}
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			if owned[id] {
				return nil, fmt.Errorf("synchronized group %q of target %s resolves to %s which is not a directory", group.Path, target.Name, dir)
			}
			continue
		}
		if owned[id] {
			relatives, err := synchronizedGroupFiles(dir, group.ExplicitFolders)
			if err != nil {
				return nil, err
			}
			for _, rel := range relatives {
				if synchronizedPathExcluded(rel, exception.ExcludedFiles) {
					continue
				}
				members[filepath.Join(dir, filepath.FromSlash(rel))] = true
			}
			continue
		}
		for _, rel := range exception.IncludedFiles {
			included, err := synchronizedIncludedFiles(dir, rel)
			if err != nil {
				return nil, err
			}
			for _, path := range included {
				members[path] = true
			}
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(members))
	for path := range members {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

// exceptionFor merges every exception set of the group that applies to
// targetID. An absent target yields a zero exception, which means "no deviation
// from full folder membership".
func (g synchronizedGroup) exceptionFor(targetID string) synchronizedException {
	merged := synchronizedException{TargetID: targetID}
	for _, exception := range g.Exceptions {
		if exception.TargetID != targetID {
			continue
		}
		merged.ExcludedFiles = append(merged.ExcludedFiles, exception.ExcludedFiles...)
		merged.IncludedFiles = append(merged.IncludedFiles, exception.IncludedFiles...)
		merged.PublicHeaders = append(merged.PublicHeaders, exception.PublicHeaders...)
		merged.PrivateHeaders = append(merged.PrivateHeaders, exception.PrivateHeaders...)
	}
	return merged
}

// synchronizedGroupOwners maps a synchronized group ID to the target IDs that
// list it in fileSystemSynchronizedGroups.
func synchronizedGroupOwners(objects map[string]pbxObject) map[string]map[string]bool {
	owners := make(map[string]map[string]bool)
	for _, object := range objects {
		for _, groupID := range pbxEntryIDs(pbxEntries(object.Body), "fileSystemSynchronizedGroups") {
			if owners[groupID] == nil {
				owners[groupID] = make(map[string]bool)
			}
			owners[groupID][object.ID] = true
		}
	}
	return owners
}

// pbxGroupParents maps a child object ID to the group containing it. When
// several groups claim the same child the lowest object ID wins, so resolution
// does not depend on map iteration order.
func pbxGroupParents(objects map[string]pbxObject) map[string]string {
	parents := make(map[string]string)
	for _, object := range objects {
		fields := pbxEntries(object.Body)
		switch pbxEntryValue(fields, "isa") {
		case "PBXGroup", "PBXVariantGroup", "XCVersionGroup", isaSynchronizedGroup:
		default:
			continue
		}
		for _, child := range pbxEntryIDs(fields, "children") {
			if existing, ok := parents[child]; ok && existing <= object.ID {
				continue
			}
			parents[child] = object.ID
		}
	}
	return parents
}

// resolveSynchronizedGroupDir walks a group's parent chain up to an absolute
// directory, honouring each node's sourceTree.
func resolveSynchronizedGroupDir(objects map[string]pbxObject, parents map[string]string, id, absRoot string) (string, error) {
	var segments []string
	seen := make(map[string]bool)
	for current := id; current != ""; current = parents[current] {
		if seen[current] {
			return "", fmt.Errorf("synchronized group %s has a cyclic parent chain", id)
		}
		seen[current] = true
		object, ok := objects[current]
		if !ok {
			return "", fmt.Errorf("synchronized group %s references missing group %s", id, current)
		}
		fields := pbxEntries(object.Body)
		if path := pbxEntryValue(fields, "path"); path != "" {
			segments = append(segments, filepath.FromSlash(path))
		}
		switch tree := pbxEntryValue(fields, "sourceTree"); tree {
		case "", "<group>":
			continue
		case "SOURCE_ROOT", "SRCROOT", "PROJECT_DIR":
			return joinReversed(absRoot, segments), nil
		case "<absolute>":
			return joinReversed("", segments), nil
		default:
			return "", fmt.Errorf("synchronized group %s uses unsupported sourceTree %q", id, tree)
		}
	}
	return joinReversed(absRoot, segments), nil
}

func joinReversed(base string, segments []string) string {
	parts := make([]string, 0, len(segments)+1)
	if base != "" {
		parts = append(parts, base)
	}
	for i := len(segments) - 1; i >= 0; i-- {
		parts = append(parts, segments[i])
	}
	if len(parts) == 0 {
		return ""
	}
	return filepath.Join(parts...)
}

// synchronizedGroupFiles lists every member of a synchronized folder as a
// slash-separated path relative to dir, sorted. Directories Xcode treats as a
// single member (asset catalogs, frameworks, explicitFolders entries) are
// returned as the directory itself and not recursed into; dot files and dot
// directories are skipped the way Xcode skips them.
func synchronizedGroupFiles(dir string, explicitFolders []string) ([]string, error) {
	opaque := make(map[string]bool, len(explicitFolders))
	for _, folder := range explicitFolders {
		opaque[folder] = true
	}
	var out []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if opaque[rel] || opaqueSynchronizedDirs[strings.ToLower(filepath.Ext(name))] {
				out = append(out, rel)
				return filepath.SkipDir
			}
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// synchronizedIncludedFiles expands one inclusion-only membership exception
// entry into absolute paths. A directory entry contributes every file below it.
// An entry that no longer exists on disk contributes nothing, matching Xcode's
// tolerance for exception sets left behind by deleted files.
func synchronizedIncludedFiles(dir, rel string) ([]string, error) {
	path := filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	if opaqueSynchronizedDirs[strings.ToLower(filepath.Ext(path))] {
		return []string{path}, nil
	}
	relatives, err := synchronizedGroupFiles(path, nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(relatives))
	for _, child := range relatives {
		out = append(out, filepath.Join(path, filepath.FromSlash(child)))
	}
	return out, nil
}

// synchronizedPathExcluded reports whether rel is covered by a membership
// exception, either exactly or as a descendant of an excluded directory.
func synchronizedPathExcluded(rel string, excluded []string) bool {
	for _, entry := range excluded {
		if entry == "" {
			continue
		}
		if rel == entry || strings.HasPrefix(rel, entry+"/") {
			return true
		}
	}
	return false
}

func normalizeSynchronizedPaths(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.Trim(filepath.ToSlash(strings.TrimSpace(value)), "/")
		if value == "" || value == "." {
			continue
		}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pbxEntries splits a PBX object body into its top-level `key = value;`
// assignments with inline /* comments */ stripped. Unlike the line-anchored
// field regexes in project_config.go this also reads the compact one-line form
// Xcode writes for synchronized root groups (spotube's HomePlayerWidget group is
// a single line), where every field but the first shares a line. The first
// occurrence of a key wins, matching the regex helpers.
func pbxEntries(body string) map[string]string {
	entries := make(map[string]string)
	key := ""
	var buf strings.Builder
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inString {
			buf.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '/' && i+1 < len(body) && body[i+1] == '*' {
			if end := strings.Index(body[i+2:], "*/"); end >= 0 {
				i += 2 + end + 1
				continue
			}
			break
		}
		if c == '"' {
			inString = true
			buf.WriteByte(c)
			continue
		}
		switch c {
		case '{', '(':
			depth++
			buf.WriteByte(c)
		case '}', ')':
			depth--
			buf.WriteByte(c)
		case '=':
			if depth == 0 && key == "" {
				key = strings.TrimSpace(buf.String())
				buf.Reset()
				continue
			}
			buf.WriteByte(c)
		case ';':
			if depth == 0 {
				if key != "" {
					if _, exists := entries[key]; !exists {
						entries[key] = strings.TrimSpace(buf.String())
					}
				}
				key = ""
				buf.Reset()
				continue
			}
			buf.WriteByte(c)
		default:
			buf.WriteByte(c)
		}
	}
	return entries
}

// pbxEntryValue returns a scalar field, unquoted.
func pbxEntryValue(entries map[string]string, key string) string {
	return cleanPBXValue(entries[key])
}

// pbxEntryID returns the object ID of a scalar reference field.
func pbxEntryID(entries map[string]string, key string) string {
	fields := strings.Fields(pbxEntryValue(entries, key))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// pbxEntryList returns the elements of a parenthesised list field, unquoted.
func pbxEntryList(entries map[string]string, key string) []string {
	value := strings.TrimSpace(entries[key])
	if len(value) < 2 || value[0] != '(' || value[len(value)-1] != ')' {
		return nil
	}
	inner := value[1 : len(value)-1]
	var out []string
	var buf strings.Builder
	depth := 0
	inString := false
	escaped := false
	flush := func() {
		if element := cleanPBXValue(buf.String()); element != "" {
			out = append(out, element)
		}
		buf.Reset()
	}
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if inString {
			buf.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			buf.WriteByte(c)
			continue
		}
		switch c {
		case '(', '{':
			depth++
			buf.WriteByte(c)
		case ')', '}':
			depth--
			buf.WriteByte(c)
		case ',':
			if depth == 0 {
				flush()
				continue
			}
			buf.WriteByte(c)
		default:
			buf.WriteByte(c)
		}
	}
	flush()
	return out
}

// pbxEntryIDs returns the 24-hex object IDs of a parenthesised reference list.
func pbxEntryIDs(entries map[string]string, key string) []string {
	values := pbxEntryList(entries, key)
	out := make([]string, 0, len(values))
	for _, value := range values {
		fields := strings.Fields(value)
		if len(fields) == 0 || !isPBXObjectID(fields[0]) {
			continue
		}
		out = append(out, fields[0])
	}
	return out
}

func isPBXObjectID(value string) bool {
	if len(value) != 24 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'A' && r <= 'F':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
