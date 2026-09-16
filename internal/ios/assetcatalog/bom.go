package assetcatalog

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// BOM ("BOMStore") container primitives. All BOM-level scalars are big endian;
// the CoreUI payloads stored inside individual blocks are little endian.
//
// Layout of a written file:
//
//	0x000  bom_header (32 bytes)
//	0x020  padding to the first data block
//	...    data blocks, each 4-byte aligned
//	...    variables section (bom_variables + bom_variable entries)
//	...    index section (bom_index_header + bom_index[count])
//	...    free list (bom_index_header + bom_index[freeCount])
//
// Apple's own archives place the variables before the index section and start
// the data at offset 512; both are reproduced here so that the resulting file
// is byte-structurally indistinguishable from an `actool` archive.

const (
	bomMagic       = "BOMStore"
	bomHeaderSize  = 32
	bomDataStart   = 512
	bomFreeEntries = 2
	// bomTreeNodeSize is the node block size Apple records in tree headers.
	// CoreUI maps a tree node by asking the BOM stream for exactly this many
	// bytes, so every leaf block must be at least this long.
	bomTreeNodeSize = 4096
	// bomTreeHeaderSize is the size of Apple's bom_tree structure: magic,
	// version, child index, node block size, path count, a flag byte and the
	// fixed key and value sizes.
	bomTreeHeaderSize = 29
	// bomTreeEntryHeader is the size of a bom_tree_entry node header:
	// is_leaf, count, forward and backward links.
	bomTreeEntryHeader = 12
	// bomTreeEntrySize is the size of one indexed key/value pair.
	bomTreeEntrySize = 8
	// bomVariableKeys marks a tree whose keys are variable-length blobs.
	bomVariableKeys = -1
)

// bomBuilder accumulates indexed data blocks, named variables and BOM trees.
type bomBuilder struct {
	// blocks[0] is always the reserved null block (address 0, length 0).
	blocks [][]byte
	vars   []bomVar
}

type bomVar struct {
	name  string
	index uint32
}

func newBOMBuilder() *bomBuilder {
	return &bomBuilder{blocks: [][]byte{nil}}
}

// addBlock stores data and returns its index. Zero-length blocks still consume
// an index, matching Apple's tree-entry placeholders.
func (b *bomBuilder) addBlock(data []byte) uint32 {
	b.blocks = append(b.blocks, data)
	return uint32(len(b.blocks) - 1)
}

// reserveBlock allocates an index whose contents are filled in later.
func (b *bomBuilder) reserveBlock() uint32 {
	return b.addBlock(nil)
}

func (b *bomBuilder) setBlock(index uint32, data []byte) {
	b.blocks[index] = data
}

func (b *bomBuilder) addVariable(name string, index uint32) {
	b.vars = append(b.vars, bomVar{name: name, index: index})
}

// bomKV is one key/value pair of a BOM tree.
type bomKV struct {
	key   []byte
	value []byte
}

// addTree writes a single-leaf BOM tree variable. keySize is the fixed size of
// every key in bytes, or bomVariableKeys when keys vary in length. BOM trees
// keep their entries sorted by raw key bytes, shorter key first on a common
// prefix; CoreUI relies on that ordering for its binary search.
func (b *bomBuilder) addTree(name string, keySize int, entries []bomKV) {
	sorted := make([]bomKV, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		if c := bytes.Compare(sorted[i].key, sorted[j].key); c != 0 {
			return c < 0
		}
		return len(sorted[i].key) < len(sorted[j].key)
	})

	// The leaf block is allocated before the key/value blocks so that the
	// index ordering matches Apple's (tree header, leaf, then pairs).
	leaf := b.reserveBlock()
	tree := make([]byte, bomTreeHeaderSize)
	copy(tree[0:4], "tree")
	binary.BigEndian.PutUint32(tree[4:8], 1)
	binary.BigEndian.PutUint32(tree[8:12], leaf)
	binary.BigEndian.PutUint32(tree[12:16], bomTreeNodeSize)
	binary.BigEndian.PutUint32(tree[16:20], uint32(len(sorted)))
	tree[20] = 0
	binary.BigEndian.PutUint32(tree[21:25], uint32(int32(keySize)))
	binary.BigEndian.PutUint32(tree[25:29], 0)
	treeIndex := b.addBlock(tree)

	// A tree leaf has two parallel representations of its keys, and CoreUI
	// uses the second one:
	//
	//   - the (value, key) block-index pairs, which BOM-level tools follow;
	//   - for a fixed-key-size tree, an *inline* key array stored inside the
	//     leaf itself, immediately after the pair array, 4-byte aligned. CoreUI
	//     reads rendition keys straight out of this array and never dereferences
	//     the key blocks.
	//
	// Omitting the inline array is silent corruption rather than a parse
	// failure: every key reads back as zeroes, so `assetutil` reports one
	// arbitrary rendition name repeated with "Asset Image Missing" for each
	// entry. Measured against four `actool` archives, the array starts at
	// bomTreeEntryHeader + bomTreeEntrySize*count rounded up to 8 (equivalently
	// 16 + 8*count, since the pair array always ends 4 bytes shy of an 8-byte
	// boundary), and the leaf is one whole node block plus the array.
	//
	// CoreUI also reads a node through BOMStreamGetDataPointer(blockSize), so
	// the leaf must be at least one whole node block; a leaf sized to just its
	// entries is what triggers a BOMStreamGetDataPointer buffer overflow.
	used := bomTreeEntryHeader + bomTreeEntrySize*len(sorted)
	arrayOffset := 0
	leafLen := bomTreeNodeSize
	if keySize > 0 {
		arrayOffset = (used + 7) & ^7
		leafLen = bomTreeNodeSize + keySize*len(sorted)
		if end := arrayOffset + keySize*len(sorted); end > leafLen {
			leafLen = end
		}
	} else if used > leafLen {
		leafLen = ((used + bomTreeNodeSize - 1) / bomTreeNodeSize) * bomTreeNodeSize
	}

	leafData := make([]byte, leafLen)
	binary.BigEndian.PutUint16(leafData[0:2], 1)
	binary.BigEndian.PutUint16(leafData[2:4], uint16(len(sorted)))
	for i, kv := range sorted {
		keyIndex := b.addBlock(kv.key)
		valueIndex := b.addBlock(kv.value)
		off := bomTreeEntryHeader + bomTreeEntrySize*i
		binary.BigEndian.PutUint32(leafData[off:off+4], valueIndex)
		binary.BigEndian.PutUint32(leafData[off+4:off+8], keyIndex)
		if keySize > 0 {
			copy(leafData[arrayOffset+keySize*i:], kv.key)
		}
	}
	b.setBlock(leaf, leafData)
	b.addVariable(name, treeIndex)
}

// build serialises the container.
func (b *bomBuilder) build() []byte {
	var out bytes.Buffer
	out.Write(make([]byte, bomDataStart))

	addresses := make([]uint32, len(b.blocks))
	lengths := make([]uint32, len(b.blocks))
	for i, data := range b.blocks {
		if i == 0 {
			continue
		}
		if pad := out.Len() % 4; pad != 0 {
			out.Write(make([]byte, 4-pad))
		}
		addresses[i] = uint32(out.Len())
		lengths[i] = uint32(len(data))
		out.Write(data)
		if len(data) == 0 {
			// A zero-length block still needs a distinct address so that
			// readers do not alias it with the following block.
			addresses[i] = uint32(out.Len())
		}
	}

	if pad := out.Len() % 4; pad != 0 {
		out.Write(make([]byte, 4-pad))
	}
	varsOffset := uint32(out.Len())
	var vars bytes.Buffer
	writeU32BE(&vars, uint32(len(b.vars)))
	for _, v := range b.vars {
		writeU32BE(&vars, v.index)
		vars.WriteByte(byte(len(v.name)))
		vars.WriteString(v.name)
	}
	varsLen := uint32(vars.Len())
	out.Write(vars.Bytes())

	if pad := out.Len() % 4; pad != 0 {
		out.Write(make([]byte, 4-pad))
	}
	indexOffset := uint32(out.Len())
	var index bytes.Buffer
	writeU32BE(&index, uint32(len(b.blocks)))
	for i := range b.blocks {
		writeU32BE(&index, addresses[i])
		writeU32BE(&index, lengths[i])
	}
	// Free list: an index header plus two empty entries.
	writeU32BE(&index, 0)
	for i := 0; i < bomFreeEntries; i++ {
		writeU32BE(&index, 0)
		writeU32BE(&index, 0)
	}
	indexLen := uint32(index.Len())
	out.Write(index.Bytes())

	buf := out.Bytes()
	copy(buf[0:8], bomMagic)
	binary.BigEndian.PutUint32(buf[8:12], 1)
	binary.BigEndian.PutUint32(buf[12:16], uint32(len(b.blocks)))
	binary.BigEndian.PutUint32(buf[16:20], indexOffset)
	binary.BigEndian.PutUint32(buf[20:24], indexLen)
	binary.BigEndian.PutUint32(buf[24:28], varsOffset)
	binary.BigEndian.PutUint32(buf[28:32], varsLen)
	return buf
}

func writeU32BE(w *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	w.Write(b[:])
}

// bomReader reads a BOM container. It exists so that the writer can be
// verified by round-tripping real archives.
type bomReader struct {
	data      []byte
	addresses []uint32
	lengths   []uint32
	vars      map[string]uint32
}

func openBOM(data []byte) (*bomReader, error) {
	if len(data) < bomHeaderSize {
		return nil, fmt.Errorf("bom: file too short (%d bytes)", len(data))
	}
	if string(data[0:8]) != bomMagic {
		return nil, fmt.Errorf("bom: bad magic %q", data[0:8])
	}
	if v := binary.BigEndian.Uint32(data[8:12]); v != 1 {
		return nil, fmt.Errorf("bom: unsupported version %d", v)
	}
	indexOffset := binary.BigEndian.Uint32(data[16:20])
	varsOffset := binary.BigEndian.Uint32(data[24:28])

	if int(indexOffset)+4 > len(data) {
		return nil, fmt.Errorf("bom: index offset %d out of range", indexOffset)
	}
	count := binary.BigEndian.Uint32(data[indexOffset : indexOffset+4])
	if int(indexOffset)+4+int(count)*8 > len(data) {
		return nil, fmt.Errorf("bom: index of %d entries out of range", count)
	}
	r := &bomReader{
		data:      data,
		addresses: make([]uint32, count),
		lengths:   make([]uint32, count),
		vars:      map[string]uint32{},
	}
	for i := uint32(0); i < count; i++ {
		off := indexOffset + 4 + i*8
		r.addresses[i] = binary.BigEndian.Uint32(data[off : off+4])
		r.lengths[i] = binary.BigEndian.Uint32(data[off+4 : off+8])
	}

	if int(varsOffset)+4 > len(data) {
		return nil, fmt.Errorf("bom: variables offset %d out of range", varsOffset)
	}
	varCount := binary.BigEndian.Uint32(data[varsOffset : varsOffset+4])
	off := int(varsOffset) + 4
	for i := uint32(0); i < varCount; i++ {
		if off+5 > len(data) {
			return nil, fmt.Errorf("bom: truncated variable %d", i)
		}
		index := binary.BigEndian.Uint32(data[off : off+4])
		nameLen := int(data[off+4])
		off += 5
		if off+nameLen > len(data) {
			return nil, fmt.Errorf("bom: truncated variable name %d", i)
		}
		r.vars[string(data[off:off+nameLen])] = index
		off += nameLen
	}
	return r, nil
}

func (r *bomReader) block(index uint32) ([]byte, error) {
	if int(index) >= len(r.addresses) {
		return nil, fmt.Errorf("bom: index %d out of range (%d blocks)", index, len(r.addresses))
	}
	start := r.addresses[index]
	length := r.lengths[index]
	if int(start)+int(length) > len(r.data) {
		return nil, fmt.Errorf("bom: block %d [%d+%d] outside file", index, start, length)
	}
	return r.data[start : start+length], nil
}

func (r *bomReader) variable(name string) ([]byte, error) {
	index, ok := r.vars[name]
	if !ok {
		return nil, fmt.Errorf("bom: no variable %q", name)
	}
	return r.block(index)
}

// tree returns the key/value pairs of the named tree variable, in stored order.
func (r *bomReader) tree(name string) ([]bomKV, error) {
	head, err := r.variable(name)
	if err != nil {
		return nil, err
	}
	if len(head) < bomTreeHeaderSize || string(head[0:4]) != "tree" {
		return nil, fmt.Errorf("bom: variable %q is not a tree", name)
	}
	child := binary.BigEndian.Uint32(head[8:12])

	var out []bomKV
	for child != 0 {
		leaf, err := r.block(child)
		if err != nil {
			return nil, err
		}
		if len(leaf) < bomTreeEntryHeader {
			return nil, fmt.Errorf("bom: tree %q has a truncated node", name)
		}
		isLeaf := binary.BigEndian.Uint16(leaf[0:2]) != 0
		count := int(binary.BigEndian.Uint16(leaf[2:4]))
		if !isLeaf {
			if count == 0 {
				return nil, fmt.Errorf("bom: tree %q has an empty interior node", name)
			}
			child = binary.BigEndian.Uint32(leaf[bomTreeEntryHeader : bomTreeEntryHeader+4])
			continue
		}
		if len(leaf) < bomTreeEntryHeader+bomTreeEntrySize*count {
			return nil, fmt.Errorf("bom: tree %q node holds %d entries but is %d bytes", name, count, len(leaf))
		}
		for i := 0; i < count; i++ {
			off := bomTreeEntryHeader + bomTreeEntrySize*i
			valueIndex := binary.BigEndian.Uint32(leaf[off : off+4])
			keyIndex := binary.BigEndian.Uint32(leaf[off+4 : off+8])
			key, err := r.block(keyIndex)
			if err != nil {
				return nil, err
			}
			value, err := r.block(valueIndex)
			if err != nil {
				return nil, err
			}
			out = append(out, bomKV{key: key, value: value})
		}
		child = binary.BigEndian.Uint32(leaf[4:8])
	}
	return out, nil
}
