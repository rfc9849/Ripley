package idevice

import (
	"fmt"
	"sort"

	"howett.net/plist"
)

// NSKeyedArchiver is Foundation's object graph serialisation. Every value
// carried by the DTX/instruments protocol is wrapped in it, so a launch request
// cannot be built without an implementation of the format.
//
// The wire shape is a binary plist with four keys:
//
//	$version   100000
//	$archiver  "NSKeyedArchiver"
//	$top       {"root": <UID>}
//	$objects   [ "$null", ... ]
//
// Every object is an entry in $objects and every reference to it is a plist UID
// (an index into that array). Scalars (strings, numbers, booleans, data) are
// stored as bare plist values; containers are dictionaries carrying a $class
// reference plus NS.keys/NS.objects.
const (
	nsKeyedArchiverVersion = 100000
	nsKeyedArchiverName    = "NSKeyedArchiver"
	nsNull                 = "$null"
)

// archiver builds an $objects table, interning strings and class descriptors so
// that a repeated key costs one UID rather than one object.
type archiver struct {
	objects  []any
	interned map[string]plist.UID
}

// archive encodes v the way Foundation would.
//
// Supported inputs are the ones the device protocols actually carry: nil,
// string, bool, the integer and float kinds, []byte, []any and map[string]any.
// Anything else is rejected loudly rather than silently archived as null.
func archive(v any) ([]byte, error) {
	a := &archiver{objects: []any{nsNull}, interned: map[string]plist.UID{}}
	root, err := a.encode(v)
	if err != nil {
		return nil, err
	}
	return plist.Marshal(map[string]any{
		"$version":  nsKeyedArchiverVersion,
		"$archiver": nsKeyedArchiverName,
		"$top":      map[string]any{"root": root},
		"$objects":  a.objects,
	}, plist.BinaryFormat)
}

// reserve appends a placeholder and returns its UID. Containers must claim their
// slot before encoding their children so that a self-referential graph still
// produces stable indices.
func (a *archiver) reserve() plist.UID {
	a.objects = append(a.objects, nsNull)
	return plist.UID(len(a.objects) - 1)
}

func (a *archiver) intern(key string, build func() any) plist.UID {
	if uid, ok := a.interned[key]; ok {
		return uid
	}
	uid := a.reserve()
	a.objects[uid] = build()
	a.interned[key] = uid
	return uid
}

// class emits (once) a $class descriptor. hierarchy is the class chain
// Foundation records, most derived first, always ending at NSObject.
func (a *archiver) class(name string, hierarchy ...string) plist.UID {
	return a.intern("$class\x00"+name, func() any {
		return map[string]any{"$classname": name, "$classes": append(hierarchy, "NSObject")}
	})
}

func (a *archiver) encode(v any) (plist.UID, error) {
	switch val := v.(type) {
	case nil:
		return 0, nil
	case string:
		return a.intern("$str\x00"+val, func() any { return val }), nil
	case bool:
		return a.reserveValue(val), nil
	case int:
		return a.reserveValue(int64(val)), nil
	case int32:
		return a.reserveValue(int64(val)), nil
	case int64:
		return a.reserveValue(val), nil
	case uint32:
		return a.reserveValue(uint64(val)), nil
	case uint64:
		return a.reserveValue(val), nil
	case float64:
		return a.reserveValue(val), nil
	case []byte:
		return a.reserveValue(val), nil
	case []any:
		return a.encodeArray(val)
	case map[string]any:
		return a.encodeDict(val)
	default:
		return 0, fmt.Errorf("nskeyedarchiver: cannot archive %T", v)
	}
}

func (a *archiver) reserveValue(v any) plist.UID {
	uid := a.reserve()
	a.objects[uid] = v
	return uid
}

func (a *archiver) encodeArray(items []any) (plist.UID, error) {
	uid := a.reserve()
	refs := make([]plist.UID, 0, len(items))
	for _, item := range items {
		ref, err := a.encode(item)
		if err != nil {
			return 0, err
		}
		refs = append(refs, ref)
	}
	a.objects[uid] = map[string]any{
		"$class":     a.class("NSArray", "NSArray"),
		"NS.objects": refs,
	}
	return uid, nil
}

// encodeDict archives in sorted key order. Foundation does not promise an order
// and the device does not care, but a deterministic encoding is testable.
func (a *archiver) encodeDict(m map[string]any) (plist.UID, error) {
	uid := a.reserve()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	keyRefs := make([]plist.UID, 0, len(keys))
	valRefs := make([]plist.UID, 0, len(keys))
	for _, k := range keys {
		keyRef, err := a.encode(k)
		if err != nil {
			return 0, err
		}
		valRef, err := a.encode(m[k])
		if err != nil {
			return 0, err
		}
		keyRefs = append(keyRefs, keyRef)
		valRefs = append(valRefs, valRef)
	}
	a.objects[uid] = map[string]any{
		"$class":     a.class("NSDictionary", "NSDictionary"),
		"NS.keys":    keyRefs,
		"NS.objects": valRefs,
	}
	return uid, nil
}

// NSError is a Foundation error decoded from an archive. The instruments
// services report failures this way, and the userInfo carries the only
// human-readable part.
type NSError struct {
	Domain   string
	Code     int64
	UserInfo map[string]any
}

func (e *NSError) Error() string {
	if desc, ok := e.UserInfo["NSLocalizedDescription"].(string); ok && desc != "" {
		return fmt.Sprintf("%s (%s %d)", desc, e.Domain, e.Code)
	}
	return fmt.Sprintf("%s error %d", e.Domain, e.Code)
}

// unarchive decodes an NSKeyedArchiver payload into plain Go values.
//
// Containers become []any / map[string]any, NSError becomes *NSError, and any
// class this code does not model is returned as a map of its decoded fields so
// that information is degraded rather than lost.
func unarchive(data []byte) (any, error) {
	var doc struct {
		Version  int            `plist:"$version"`
		Archiver string         `plist:"$archiver"`
		Top      map[string]any `plist:"$top"`
		Objects  []any          `plist:"$objects"`
	}
	if _, err := plist.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("nskeyedarchiver: decode: %w", err)
	}
	if doc.Archiver != nsKeyedArchiverName {
		return nil, fmt.Errorf("nskeyedarchiver: unexpected archiver %q", doc.Archiver)
	}
	if len(doc.Objects) == 0 {
		return nil, fmt.Errorf("nskeyedarchiver: empty $objects")
	}
	u := &unarchiver{objects: doc.Objects, active: map[plist.UID]bool{}}

	root, ok := doc.Top["root"]
	if !ok {
		// Some senders name the single top-level object differently; with
		// exactly one entry there is no ambiguity about which one it is.
		for _, v := range doc.Top {
			root, ok = v, true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("nskeyedarchiver: $top has no root")
	}
	uid, ok := asUID(root)
	if !ok {
		return root, nil
	}
	return u.decode(uid)
}

type unarchiver struct {
	objects []any
	active  map[plist.UID]bool
}

func asUID(v any) (plist.UID, bool) {
	switch n := v.(type) {
	case plist.UID:
		return n, true
	case uint64:
		return plist.UID(n), true
	default:
		return 0, false
	}
}

func (u *unarchiver) decode(uid plist.UID) (any, error) {
	if int(uid) >= len(u.objects) {
		return nil, fmt.Errorf("nskeyedarchiver: UID %d out of range", uid)
	}
	if uid == 0 {
		return nil, nil
	}
	if u.active[uid] {
		return nil, fmt.Errorf("nskeyedarchiver: cyclic reference at UID %d", uid)
	}
	u.active[uid] = true
	defer delete(u.active, uid)

	switch obj := u.objects[uid].(type) {
	case string:
		if obj == nsNull {
			return nil, nil
		}
		return obj, nil
	case map[string]any:
		return u.decodeObject(obj)
	default:
		return obj, nil
	}
}

func (u *unarchiver) decodeObject(obj map[string]any) (any, error) {
	class := u.className(obj["$class"])
	switch class {
	case "NSDictionary", "NSMutableDictionary":
		return u.decodeDict(obj)
	case "NSArray", "NSMutableArray", "NSSet", "NSMutableSet":
		return u.decodeArray(obj)
	case "NSError":
		return u.decodeError(obj)
	case "NSDate":
		return obj["NS.time"], nil
	case "NSData", "NSMutableData":
		return obj["NS.data"], nil
	}
	// Unknown class: hand back every field, resolved.
	out := map[string]any{}
	if class != "" {
		out["$class"] = class
	}
	for k, v := range obj {
		if k == "$class" {
			continue
		}
		resolved, err := u.resolve(v)
		if err != nil {
			return nil, err
		}
		out[k] = resolved
	}
	return out, nil
}

// resolve turns a field value into a Go value, following UIDs.
func (u *unarchiver) resolve(v any) (any, error) {
	if uid, ok := asUID(v); ok {
		return u.decode(uid)
	}
	if list, ok := v.([]any); ok {
		out := make([]any, 0, len(list))
		for _, item := range list {
			resolved, err := u.resolve(item)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	}
	return v, nil
}

func (u *unarchiver) className(v any) string {
	uid, ok := asUID(v)
	if !ok || int(uid) >= len(u.objects) {
		return ""
	}
	desc, ok := u.objects[uid].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := desc["$classname"].(string)
	return name
}

func (u *unarchiver) refs(v any) ([]plist.UID, error) {
	list, ok := v.([]any)
	if !ok {
		if v == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("nskeyedarchiver: expected an array of references, got %T", v)
	}
	out := make([]plist.UID, 0, len(list))
	for _, item := range list {
		uid, ok := asUID(item)
		if !ok {
			return nil, fmt.Errorf("nskeyedarchiver: expected a reference, got %T", item)
		}
		out = append(out, uid)
	}
	return out, nil
}

func (u *unarchiver) decodeDict(obj map[string]any) (any, error) {
	keys, err := u.refs(obj["NS.keys"])
	if err != nil {
		return nil, err
	}
	values, err := u.refs(obj["NS.objects"])
	if err != nil {
		return nil, err
	}
	if len(keys) != len(values) {
		return nil, fmt.Errorf("nskeyedarchiver: dictionary has %d keys but %d values", len(keys), len(values))
	}
	out := make(map[string]any, len(keys))
	for i, keyRef := range keys {
		key, err := u.decode(keyRef)
		if err != nil {
			return nil, err
		}
		value, err := u.decode(values[i])
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			name = fmt.Sprint(key)
		}
		out[name] = value
	}
	return out, nil
}

func (u *unarchiver) decodeArray(obj map[string]any) (any, error) {
	refs, err := u.refs(obj["NS.objects"])
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		item, err := u.decode(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func (u *unarchiver) decodeError(obj map[string]any) (any, error) {
	e := &NSError{}
	if code, ok := toInt64(obj["NSCode"]); ok {
		e.Code = code
	}
	domain, err := u.resolve(obj["NSDomain"])
	if err != nil {
		return nil, err
	}
	e.Domain, _ = domain.(string)
	info, err := u.resolve(obj["NSUserInfo"])
	if err != nil {
		return nil, err
	}
	if m, ok := info.(map[string]any); ok {
		e.UserInfo = m
	} else {
		e.UserInfo = map[string]any{}
	}
	return e, nil
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}
