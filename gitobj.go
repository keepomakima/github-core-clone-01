// Git object model, pkt-line framing and packfiles: the wire formats the
// smart-HTTP surface speaks. Standard library only (crypto/sha1,
// compress/zlib). Everything here is written from the git protocol as
// documented in the kit (pkt-line framing, v2/v0 advertisements, pack v2
// layout, delta format).

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

const zeroOID = "0000000000000000000000000000000000000000"
const pktLineMax = 65520

type gitObjectType string

const (
	objCommit gitObjectType = "commit"
	objTree   gitObjectType = "tree"
	objBlob   gitObjectType = "blob"
	objTag    gitObjectType = "tag"
)

type gitObject struct {
	typ  gitObjectType
	data []byte
}

var typeCode = map[gitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]gitObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func isObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func hashObject(typ gitObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	out = append(out, payload...)
	return out
}

func pktLineStr(payload string) []byte { return pktLine([]byte(payload)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktItem is one element of a pkt-lines sequence.
type pktItem struct {
	kind    string // "data", "flush", "delim"
	payload []byte
}

func pktLineItems(items []pktItem) []byte {
	var out []byte
	for _, item := range items {
		switch item.kind {
		case "flush":
			out = append(out, pktFlush...)
		case "delim":
			out = append(out, pktDelim...)
		default:
			out = append(out, pktLine(item.payload)...)
		}
	}
	return out
}

func pktData(s string) pktItem  { return pktItem{kind: "data", payload: []byte(s)} }
func pktFlushItem() pktItem     { return pktItem{kind: "flush"} }
func pktDelimItem() pktItem     { return pktItem{kind: "delim"} }

type pkt struct {
	kind    string // "data", "flush", "delim", "response-end"
	payload []byte
}

// decodePktLines decodes a whole buffer into pkt-lines. It returns the
// packets plus the offset just past the last consumed packet (the bytes after
// it are raw pack data). A malformed header stops decoding; callers that must
// survive garbage catch it.
func decodePktLines(buffer []byte) (packets []pkt, consumed int) {
	offset := 0
	isHex := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
	}
	for offset+4 <= len(buffer) {
		head := buffer[offset : offset+4]
		if !isHex(head[0]) || !isHex(head[1]) || !isHex(head[2]) || !isHex(head[3]) {
			break
		}
		length := 0
		for _, c := range head {
			length *= 16
			switch {
			case c >= '0' && c <= '9':
				length += int(c - '0')
			case c >= 'a' && c <= 'f':
				length += int(c-'a') + 10
			default:
				length += int(c-'A') + 10
			}
		}
		if length == 0 {
			packets = append(packets, pkt{kind: "flush"})
			offset += 4
			continue
		}
		if length == 1 {
			packets = append(packets, pkt{kind: "delim"})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, pkt{kind: "response-end"})
			offset += 4
			continue
		}
		if length < 4 || length > pktLineMax || offset+length > len(buffer) {
			break
		}
		packets = append(packets, pkt{kind: "data", payload: buffer[offset+4 : offset+length]})
		offset += length
	}
	return packets, offset
}

// startsWithPktLine is true when buffer starts with a valid pkt-line header
// (not raw pack data).
func startsWithPktLine(buffer []byte) bool {
	if len(buffer) < 4 {
		return false
	}
	isHex := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
	}
	return isHex(buffer[0]) && isHex(buffer[1]) && isHex(buffer[2]) && isHex(buffer[3])
}

// ---------------------------------------------------------------------------------------------
// object parsing / building
// ---------------------------------------------------------------------------------------------

type treeEntry struct {
	mode string
	name string
	oid  string
}

// Parsed-commit/tree caches: a git object is immutable once stored, so a
// parse is pure per data bytes. planFetch, the reachability walks and the
// REST surface parse the same commits/trees several times per request; the
// cache turns every repeat into a lookup. All callers run under the dispatch
// mutex, so plain maps are safe.
var commitParseCache = map[string]*commitView{}
var treeParseCache = map[string][]treeEntry{}

func cachedParseCommit(data []byte) *commitView {
	key := string(data)
	if hit, ok := commitParseCache[key]; ok {
		return hit
	}
	hit := parseCommit(data)
	commitParseCache[key] = hit
	return hit
}

func cachedParseTree(data []byte) []treeEntry {
	key := string(data)
	if hit, ok := treeParseCache[key]; ok {
		return hit
	}
	hit := parseTree(data)
	treeParseCache[key] = hit
	return hit
}

func parseTree(data []byte) []treeEntry {
	var entries []treeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			panic("malformed tree object")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			panic("malformed tree object")
		}
		nul += space + 1
		if nul+21 > len(data) {
			panic("malformed tree object")
		}
		entries = append(entries, treeEntry{
			mode: string(data[offset:space]),
			name: string(data[space+1 : nul]),
			oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries
}

func compareTreeNames(a, b treeEntry) int {
	left := a.name
	if a.mode == "40000" {
		left = a.name + "/"
	}
	right := b.name
	if b.mode == "40000" {
		right = b.name + "/"
	}
	return bytes.Compare([]byte(left), []byte(right))
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var out []byte
	for _, entry := range sorted {
		out = append(out, []byte(entry.mode+" "+entry.name+"\x00")...)
		raw, _ := hex.DecodeString(entry.oid)
		out = append(out, raw...)
	}
	return out
}

type commitView struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) *commitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	view := &commitView{}
	for _, line := range bytes.Split(data[:headerEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		s := string(line)
		space := indexByte(s, ' ')
		key := s
		value := ""
		if space >= 0 {
			key = s[:space]
			value = s[space+1:]
		}
		switch key {
		case "tree":
			view.tree = value
		case "parent":
			view.parents = append(view.parents, value)
		case "author":
			view.author = value
		case "committer":
			view.committer = value
		}
	}
	view.message = message
	return view
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func buildCommit(tree string, parents []string, message, author, committer string, timestamp int64) []byte {
	var out string
	out += "tree " + tree + "\n"
	for _, parent := range parents {
		out += "parent " + parent + "\n"
	}
	out += fmt.Sprintf("author %s %d +0000\n", author, timestamp)
	out += fmt.Sprintf("committer %s %d +0000\n", committer, timestamp)
	out += "\n"
	out += message
	return []byte(out)
}

type tagView struct {
	object  string
	typ     gitObjectType
	tag     string
	tagger  string
	message string
}

func parseTag(data []byte) *tagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := map[string]string{}
	for _, line := range bytes.Split(data[:headerEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		s := string(line)
		space := indexByte(s, ' ')
		if space < 0 {
			continue
		}
		headers[s[:space]] = s[space+1:]
	}
	typ := gitObjectType(headers["type"])
	if typ == "" {
		typ = objCommit
	}
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	return &tagView{
		object:  headers["object"],
		typ:     typ,
		tag:     headers["tag"],
		tagger:  headers["tagger"],
		message: message,
	}
}

// ---------------------------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------------------------

func writePackObjectHeader(typ gitObjectType, size int) []byte {
	code := typeCode[typ]
	var bytesOut []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining = remaining / 16
	for remaining > 0 {
		bytesOut = append(bytesOut, byte(first|0x80))
		first = remaining & 0x7f
		remaining = remaining / 128
	}
	bytesOut = append(bytesOut, byte(first))
	return bytesOut
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// writePack builds a packfile from plain objects (no deltas).
func writePack(objects []*gitObject) []byte {
	var parts []byte
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(objects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	parts = append(parts, header...)
	for _, object := range objects {
		parts = append(parts, writePackObjectHeader(object.typ, len(object.data))...)
		parts = append(parts, deflate(object.data)...)
	}
	sum := sha1.Sum(parts)
	parts = append(parts, sum[:]...)
	return parts
}

func readVarintSize(buffer []byte, start int) (size int, next int, first int) {
	b := int(buffer[start])
	first = b
	size = b & 0x0f
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		b = int(buffer[offset])
		offset++
		size += (b & 0x7f) << shift
		shift += 7
	}
	return size, offset, first
}

func readOfsDeltaBase(buffer []byte, start int) (baseOffset int, next int) {
	b := int(buffer[start])
	offset := start + 1
	value := b & 0x7f
	for b&0x80 != 0 {
		b = int(buffer[offset])
		offset++
		value = ((value + 1) * 128) + (b & 0x7f)
	}
	return value, offset
}

// inflateAt inflates one zlib stream starting at offset (ending before end)
// and returns the data plus the offset just past the stream.
func inflateAt(buffer []byte, offset, end int) ([]byte, int, error) {
	r := bytes.NewReader(buffer[offset:end])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := (end - offset) - r.Len()
	return data, offset + consumed, nil
}

// applyDelta applies a git delta instruction stream to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := int(delta[offset])
			offset++
			value += (b & 0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := int(delta[offset])
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := op & 0x7f
			copy(out[written:written+literal], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type packReadResult struct {
	objects    map[string]*gitObject
	deltaCount int
}

// readPack reads a packfile (version 2/3), resolving OFS_DELTA and REF_DELTA
// entries. resolveBase supplies a base a thin pack leaves out. It errors on
// corruption.
func readPack(pack []byte, resolveBase func(oid string) *gitObject) (*packReadResult, error) {
	buffer := pack
	if len(buffer) < 32 || string(buffer[0:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := int(buffer[4])<<24 | int(buffer[5])<<16 | int(buffer[6])<<8 | int(buffer[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(buffer[8])<<24 | int(buffer[9])<<16 | int(buffer[10])<<8 | int(buffer[11])
	trailerStart := len(buffer) - 20
	trailer := hex.EncodeToString(buffer[trailerStart:])
	sum := sha1.Sum(buffer[:trailerStart])
	if trailer != hex.EncodeToString(sum[:]) {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	type resolvedObject struct {
		typ  gitObjectType
		data []byte
		oid  string
	}
	type rawEntry struct {
		offset     int
		typeCode   int
		size       int
		data       []byte
		baseOffset int
		baseOid    string
		resolved   *resolvedObject
	}
	raw := make([]*rawEntry, 0, declaredCount)
	byOffset := map[int]*rawEntry{}
	offset := 12
	for index := 0; index < declaredCount; index++ {
		entryOffset := offset
		size, next, first := readVarintSize(buffer, offset)
		offset = next
		tc := (first >> 4) & 0x07
		entry := &rawEntry{offset: entryOffset, typeCode: tc, size: size, data: []byte{}}
		if tc == 6 {
			base, nextOff := readOfsDeltaBase(buffer, offset)
			offset = nextOff
			entry.baseOffset = entryOffset - base
		} else if tc == 7 {
			entry.baseOid = hex.EncodeToString(buffer[offset : offset+20])
			offset += 20
		} else if _, ok := codeType[tc]; !ok {
			return nil, fmt.Errorf("pack entry has unknown type code %d", tc)
		}
		inflated, nextOff, err := inflateAt(buffer, offset, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}

	objects := map[string]*gitObject{}
	resolvedByOid := map[string]*rawEntry{}
	resolving := map[int]bool{}
	deltaCount := 0
	var resolve func(entry *rawEntry) (*resolvedObject, error)
	resolve = func(entry *rawEntry) (*resolvedObject, error) {
		if entry.resolved != nil {
			return entry.resolved, nil
		}
		if resolving[entry.offset] {
			return nil, errors.New("delta cycle in pack")
		}
		resolving[entry.offset] = true
		var typ gitObjectType
		var data []byte
		if entry.typeCode == 6 {
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				return nil, errors.New("ofs-delta base is not in the pack")
			}
			base, err := resolve(baseEntry)
			if err != nil {
				return nil, err
			}
			typ = base.typ
			data, err = applyDelta(base.data, entry.data)
			if err != nil {
				return nil, err
			}
		} else if entry.typeCode == 7 {
			wanted := entry.baseOid
			if baseEntry, ok := resolvedByOid[wanted]; ok {
				base, err := resolve(baseEntry)
				if err != nil {
					return nil, err
				}
				typ = base.typ
				data, err = applyDelta(base.data, entry.data)
				if err != nil {
					return nil, err
				}
			} else {
				var external *gitObject
				if resolveBase != nil {
					external = resolveBase(wanted)
				}
				if external == nil {
					return nil, fmt.Errorf("ref-delta base %s is not available", wanted)
				}
				typ = external.typ
				var err error
				data, err = applyDelta(external.data, entry.data)
				if err != nil {
					return nil, err
				}
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := hashObject(typ, data)
		entry.resolved = &resolvedObject{typ: typ, data: data, oid: oid}
		resolvedByOid[oid] = entry
		delete(resolving, entry.offset)
		return entry.resolved, nil
	}
	for _, entry := range raw {
		resolved, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if entry.typeCode == 6 || entry.typeCode == 7 {
			deltaCount++
		}
		if _, ok := objects[resolved.oid]; !ok {
			objects[resolved.oid] = &gitObject{typ: resolved.typ, data: resolved.data}
		}
	}
	return &packReadResult{objects: objects, deltaCount: deltaCount}, nil
}

// ---------------------------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------------------------

type objectLookup func(oid string) *gitObject

// collectReachable returns every object reachable from roots (commits, trees,
// tags, blobs).
func collectReachable(roots []string, lookup objectLookup) (map[string]bool, error) {
	seen := map[string]bool{}
	var queue []string
	for _, root := range roots {
		if isObjectID(root) {
			queue = append(queue, root)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		object := lookup(oid)
		if object == nil {
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch object.typ {
		case objCommit:
			commit := cachedParseCommit(object.data)
			queue = append(queue, commit.tree)
			queue = append(queue, commit.parents...)
		case objTag:
			queue = append(queue, parseTag(object.data).object)
		case objTree:
			for _, entry := range cachedParseTree(object.data) {
				if entry.mode != "160000" {
					queue = append(queue, entry.oid)
				}
			}
		}
	}
	return seen, nil
}

// isAncestor is true when ancestor is reachable from tip (or equal to it).
func isAncestor(ancestor, tip string, lookup objectLookup) bool {
	if ancestor == tip {
		return true
	}
	seen := map[string]bool{tip: true}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		object := lookup(oid)
		if object == nil {
			continue
		}
		if object.typ == objCommit {
			for _, parent := range cachedParseCommit(object.data).parents {
				if parent == ancestor {
					return true
				}
				if !seen[parent] {
					seen[parent] = true
					queue = append(queue, parent)
				}
			}
		} else if object.typ == objTag {
			target := parseTag(object.data).object
			if target == ancestor {
				return true
			}
			if !seen[target] {
				seen[target] = true
				queue = append(queue, target)
			}
		}
	}
	return false
}

// mergeBase returns the first common ancestor commit of a and b (the
// merge-base), walking parents breadth-first from both tips. Empty string
// when the histories are disjoint.
func mergeBase(a, b string, lookup objectLookup) string {
	ancestorsOf := func(tip string) map[string]bool {
		seen := map[string]bool{}
		queue := []string{tip}
		for len(queue) > 0 {
			oid := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if seen[oid] {
				continue
			}
			seen[oid] = true
			object := lookup(oid)
			if object != nil && object.typ == objCommit {
				queue = append(queue, cachedParseCommit(object.data).parents...)
			}
		}
		return seen
	}
	setA := ancestorsOf(a)
	queue := []string{b}
	seenB := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if setA[oid] {
			return oid
		}
		object := lookup(oid)
		if object != nil && object.typ == objCommit {
			queue = append(queue, cachedParseCommit(object.data).parents...)
		}
	}
	return ""
}
