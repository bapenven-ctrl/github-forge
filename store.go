package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Deterministic logical clock: a fixed base plus one second per event, so no
// timestamp the forge emits ever depends on the wall clock.
// ---------------------------------------------------------------------------

type LogicalClock struct {
	mu    sync.Mutex
	value int64
}

func (c *LogicalClock) Tick() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value++
	return c.value
}

func (c *LogicalClock) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

var CLOCK = &LogicalClock{value: 1739175597}

func isoOf(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func gitStamp(ts int64) string {
	return fmt.Sprintf("%d +0000", ts)
}

func viewerIdent(ts int64) string {
	return fmt.Sprintf("%s <%s> %s", VIEWER_NAME, VIEWER_EMAIL, gitStamp(ts))
}

// ---------------------------------------------------------------------------
// Git object store (in memory)
// ---------------------------------------------------------------------------

var TYPE_CODES = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var CODE_TYPES = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

type GitObject struct {
	Type string
	Data []byte
}

type ObjectStore struct {
	mu      sync.RWMutex
	objects map[string]*GitObject
}

var STORE = &ObjectStore{objects: map[string]*GitObject{}}

func (s *ObjectStore) Put(otype string, data []byte) string {
	header := fmt.Sprintf("%s %d", otype, len(data))
	h := sha1.New()
	h.Write([]byte(header))
	h.Write([]byte{0})
	h.Write(data)
	oid := hex.EncodeToString(h.Sum(nil))
	s.mu.Lock()
	if _, ok := s.objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		s.objects[oid] = &GitObject{Type: otype, Data: cp}
	}
	s.mu.Unlock()
	return oid
}

func (s *ObjectStore) Get(oid string) (*GitObject, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[oid]
	return obj, ok
}

func (s *ObjectStore) Has(oid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.objects[oid]
	return ok
}

func (s *ObjectStore) TypeOf(oid string) string {
	if obj, ok := s.Get(oid); ok {
		return obj.Type
	}
	return ""
}

func (s *ObjectStore) All() map[string]*GitObject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*GitObject, len(s.objects))
	for k, v := range s.objects {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// Commits and trees
// ---------------------------------------------------------------------------

type Commit struct {
	Tree     string
	Parents  []string
	Author   string
	Committer string
	Message  string
}

func parseCommit(data []byte) *Commit {
	head := data
	message := ""
	if idx := bytes.Index(data, []byte("\n\n")); idx >= 0 {
		head = data[:idx]
		message = string(data[idx+2:])
	}
	c := &Commit{Message: message}
	for _, line := range strings.Split(string(head), "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, line[7:])
		case strings.HasPrefix(line, "author "):
			c.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.Committer = line[10:]
		}
	}
	return c
}

func buildCommit(treeOid string, parents []string, author, committer, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + treeOid + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + "\n")
	b.WriteString("committer " + committer + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

func commitTs(oid string) int64 {
	obj, ok := STORE.Get(oid)
	if !ok || obj.Type != "commit" {
		return 0
	}
	committer := parseCommit(obj.Data).Committer
	fields := strings.Fields(committer)
	if len(fields) < 2 {
		return 0
	}
	ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	pos := 0
	for pos < len(data) {
		space := bytes.IndexByte(data[pos:], ' ')
		if space < 0 {
			break
		}
		space += pos
		mode := string(data[pos:space])
		nul := bytes.IndexByte(data[space:], 0)
		if nul < 0 {
			break
		}
		nul += space
		name := string(data[space+1 : nul])
		if nul+21 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, Oid: oid})
		pos = nul + 21
	}
	return entries
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		ki := sorted[i].Name
		if sorted[i].Mode == "40000" {
			ki += "/"
		}
		kj := sorted[j].Name
		if sorted[j].Mode == "40000" {
			kj += "/"
		}
		return ki < kj
	})
	var out bytes.Buffer
	for _, e := range sorted {
		out.WriteString(e.Mode)
		out.WriteByte(' ')
		out.WriteString(e.Name)
		out.WriteByte(0)
		raw, _ := hex.DecodeString(e.Oid)
		out.Write(raw)
	}
	return out.Bytes()
}

// flat path -> (mode, blob oid)
type FlatEntry struct {
	Mode string
	Oid  string
}

func flattenTree(treeOid string, prefix string) map[string]FlatEntry {
	flat := map[string]FlatEntry{}
	if treeOid == "" {
		return flat
	}
	obj, ok := STORE.Get(treeOid)
	if !ok || obj.Type != "tree" {
		return flat
	}
	for _, e := range parseTree(obj.Data) {
		path := prefix + e.Name
		if e.Mode == "40000" {
			for k, v := range flattenTree(e.Oid, path+"/") {
				flat[k] = v
			}
		} else {
			flat[path] = FlatEntry{Mode: e.Mode, Oid: e.Oid}
		}
	}
	return flat
}

type treeNode struct {
	children map[string]*treeNode
	leaf     *FlatEntry
}

func treeFromFlat(flat map[string]FlatEntry) string {
	root := &treeNode{children: map[string]*treeNode{}}
	for path, value := range flat {
		parts := strings.Split(path, "/")
		node := root
		for _, part := range parts[:len(parts)-1] {
			child, ok := node.children[part]
			if !ok {
				child = &treeNode{children: map[string]*treeNode{}}
				node.children[part] = child
			}
			node = child
		}
		leaf := value
		node.children[parts[len(parts)-1]] = &treeNode{children: map[string]*treeNode{}, leaf: &leaf}
	}
	var emit func(n *treeNode) string
	emit = func(n *treeNode) string {
		var entries []TreeEntry
		for name, child := range n.children {
			if child.leaf != nil && len(child.children) == 0 {
				entries = append(entries, TreeEntry{Mode: child.leaf.Mode, Name: name, Oid: child.leaf.Oid})
			} else {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: emit(child)})
			}
		}
		return STORE.Put("tree", buildTree(entries))
	}
	return emit(root)
}

func commitTreeOid(commitOid string) string {
	obj, ok := STORE.Get(commitOid)
	if !ok || obj.Type != "commit" {
		return ""
	}
	return parseCommit(obj.Data).Tree
}

func commitFlat(commitOid string) map[string]FlatEntry {
	if commitOid == "" {
		return map[string]FlatEntry{}
	}
	return flattenTree(commitTreeOid(commitOid), "")
}

func ancestorsOf(tip string) []string {
	var order []string
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] || !STORE.Has(oid) {
			continue
		}
		seen[oid] = true
		order = append(order, oid)
		obj, _ := STORE.Get(oid)
		parents := parseCommit(obj.Data).Parents
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	return order
}

// isAncestor reports whether target appears in tip's ancestry, tip itself
// included. It walks parents breadth-first but stops the moment target is
// found, so the common fast-forward check (target is the direct parent)
// costs one step instead of a full ancestry listing.
func isAncestor(tip, target string) bool {
	if tip == "" || target == "" {
		return false
	}
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == target {
			return true
		}
		if seen[oid] || !STORE.Has(oid) {
			continue
		}
		seen[oid] = true
		obj, _ := STORE.Get(oid)
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return false
}

func mergeBaseOf(aOid, bOid string) string {
	if aOid == "" || bOid == "" {
		return ""
	}
	aAncestors := map[string]bool{}
	for _, oid := range ancestorsOf(aOid) {
		aAncestors[oid] = true
	}
	queue := []string{bOid}
	seen := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] || !STORE.Has(oid) {
			continue
		}
		seen[oid] = true
		if aAncestors[oid] {
			return oid
		}
		obj, _ := STORE.Get(oid)
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return ""
}

func treeClosure(treeOid string) []string {
	var out []string
	seen := map[string]bool{}
	stack := []string{treeOid}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] || !STORE.Has(oid) {
			continue
		}
		seen[oid] = true
		out = append(out, oid)
		obj, _ := STORE.Get(oid)
		if obj.Type == "tree" {
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.Oid)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Packfiles
// ---------------------------------------------------------------------------

func writePack(oids []string) []byte {
	var out bytes.Buffer
	out.WriteString("PACK")
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], 2)
	out.Write(tmp[:])
	binary.BigEndian.PutUint32(tmp[:], uint32(len(oids)))
	out.Write(tmp[:])
	for _, oid := range oids {
		obj, _ := STORE.Get(oid)
		data := obj.Data
		remaining := len(data) >> 4
		byte0 := byte(TYPE_CODES[obj.Type]<<4) | byte(len(data)&0x0F)
		if remaining > 0 {
			byte0 |= 0x80
		}
		out.WriteByte(byte0)
		for remaining > 0 {
			chunk := byte(remaining & 0x7F)
			remaining >>= 7
			if remaining > 0 {
				chunk |= 0x80
			}
			out.WriteByte(chunk)
		}
		var zbuf bytes.Buffer
		w := zlib.NewWriter(&zbuf)
		w.Write(data)
		w.Close()
		out.Write(zbuf.Bytes())
	}
	sum := sha1.Sum(out.Bytes())
	out.Write(sum[:])
	return out.Bytes()
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() int {
		value := 0
		shift := uint(0)
		for {
			b := delta[pos]
			pos++
			value |= int(b&0x7F) << shift
			shift += 7
			if b&0x80 == 0 {
				return value
			}
		}
	}
	sourceSize := readVarint()
	targetSize := readVarint()
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	var out bytes.Buffer
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					offset |= int(delta[pos]) << (8 * uint(bit))
					pos++
				}
			}
			for bit := 0; bit < 3; bit++ {
				if op&(0x10<<uint(bit)) != 0 {
					size |= int(delta[pos]) << (8 * uint(bit))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			out.Write(base[offset : offset+size])
		} else if op != 0 {
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if out.Len() != targetSize {
		return nil, fmt.Errorf("delta target size mismatch")
	}
	return out.Bytes(), nil
}

type pendingDelta struct {
	baseOid string
	delta   []byte
}

// packCursor is a byte-precise reader over the pack body: it implements
// io.ByteReader, so compress/flate reads exactly the bytes of one zlib
// stream and the cursor lands precisely on the next pack entry.
type packCursor struct {
	data []byte
	pos  int
}

func (c *packCursor) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.pos:])
	c.pos += n
	return n, nil
}

func (c *packCursor) ReadByte() (byte, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	b := c.data[c.pos]
	c.pos++
	return b, nil
}

func readPack(data []byte) ([]string, error) {
	if len(data) < 12 || string(data[:4]) != "PACK" {
		return nil, fmt.Errorf("not a packfile")
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	pos := 12
	var stored []string
	var pending []*pendingDelta
	offsets := map[int]string{}
	nextByte := func() (byte, error) {
		if pos >= len(data) {
			return 0, fmt.Errorf("pack truncated")
		}
		b := data[pos]
		pos++
		return b, nil
	}
	for i := 0; i < count; i++ {
		entryStart := pos
		first, err := nextByte()
		if err != nil {
			return nil, err
		}
		rawType := int((first >> 4) & 0x07)
		otype, ok := CODE_TYPES[rawType]
		if !ok {
			otype = "blob"
		}
		size := int(first & 0x0F)
		shift := uint(4)
		b := first
		for b&0x80 != 0 {
			b, err = nextByte()
			if err != nil {
				return nil, err
			}
			size |= int(b&0x7F) << shift
			shift += 7
		}
		baseOid := ""
		isDelta := false
		if rawType == 6 { // OFS_DELTA
			b, err = nextByte()
			if err != nil {
				return nil, err
			}
			offset := int(b & 0x7F)
			for b&0x80 != 0 {
				b, err = nextByte()
				if err != nil {
					return nil, err
				}
				offset = ((offset + 1) << 7) | int(b&0x7F)
			}
			baseOid = offsets[entryStart-offset]
			isDelta = true
		} else if rawType == 7 { // REF_DELTA
			if pos+20 > len(data) {
				return nil, fmt.Errorf("pack truncated")
			}
			baseOid = hex.EncodeToString(data[pos : pos+20])
			pos += 20
			isDelta = true
		}
		cursor := &packCursor{data: data, pos: pos}
		zr, err := zlib.NewReader(cursor)
		if err != nil {
			return nil, err
		}
		chunk, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
		pos = cursor.pos
		if len(chunk) != size {
			return nil, fmt.Errorf("pack object size mismatch")
		}
		if isDelta {
			pending = append(pending, &pendingDelta{baseOid: baseOid, delta: chunk})
		} else {
			newOid := STORE.Put(otype, chunk)
			offsets[entryStart] = newOid
			stored = append(stored, newOid)
		}
	}
	progress := true
	for len(pending) > 0 && progress {
		progress = false
		var rest []*pendingDelta
		for _, item := range pending {
			baseObj, has := STORE.Get(item.baseOid)
			if has {
				resolved, err := applyDelta(baseObj.Data, item.delta)
				if err != nil {
					return nil, err
				}
				stored = append(stored, STORE.Put(baseObj.Type, resolved))
				progress = true
			} else {
				rest = append(rest, item)
			}
		}
		pending = rest
	}
	return stored, nil
}

// packForFetch mirrors the v1 selection logic exactly: which objects a fetch
// sends, plus the new shallow boundary list.
func packForFetch(wants []string, haves map[string]bool, shallowRoots map[string]bool, depth *int, since *int64) ([]string, []string) {
	excluded := map[string]bool{}
	for have := range haves {
		if shallowRoots[have] {
			continue
		}
		if obj, ok := STORE.Get(have); ok && obj.Type == "commit" {
			for _, oid := range ancestorsOf(have) {
				excluded[oid] = true
				if t := commitTreeOid(oid); t != "" {
					for _, sub := range treeClosure(t) {
						excluded[sub] = true
					}
				}
			}
		}
	}
	shallowExcluded := map[string]bool{}
	for root := range shallowRoots {
		if obj, ok := STORE.Get(root); ok && obj.Type == "commit" {
			shallowExcluded[root] = true
			if t := commitTreeOid(root); t != "" {
				for _, sub := range treeClosure(t) {
					shallowExcluded[sub] = true
				}
			}
		}
	}
	type qitem struct {
		oid  string
		dist int
	}
	included := map[string]int{}
	var includedOrder []string
	var queue []qitem
	for _, want := range wants {
		if obj, ok := STORE.Get(want); ok && obj.Type == "commit" && !excluded[want] {
			queue = append(queue, qitem{want, 0})
		}
	}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		oid, dist := item.oid, item.dist
		if cur, ok := included[oid]; ok && cur <= dist {
			continue
		}
		if excluded[oid] || !STORE.Has(oid) {
			continue
		}
		if depth != nil && dist >= *depth && !shallowExcluded[oid] {
			continue
		}
		if _, ok := included[oid]; !ok {
			includedOrder = append(includedOrder, oid)
		}
		included[oid] = dist
		if depth == nil || dist < *depth {
			obj, _ := STORE.Get(oid)
			for _, parent := range parseCommit(obj.Data).Parents {
				if since != nil && STORE.Has(parent) && commitTs(parent) < *since {
					continue
				}
				queue = append(queue, qitem{parent, dist + 1})
			}
		}
	}
	var objects []string
	seen := map[string]bool{}
	add := func(oid string) {
		if !seen[oid] && !excluded[oid] && !shallowExcluded[oid] && STORE.Has(oid) {
			seen[oid] = true
			objects = append(objects, oid)
		}
	}
	var extraWants []string
	for _, want := range wants {
		if obj, ok := STORE.Get(want); ok && obj.Type != "commit" {
			extraWants = append(extraWants, want)
		}
	}
	for _, oid := range extraWants {
		obj, _ := STORE.Get(oid)
		switch obj.Type {
		case "tag":
			add(oid)
			firstLine := obj.Data
			if idx := bytes.IndexByte(firstLine, '\n'); idx >= 0 {
				firstLine = firstLine[:idx]
			}
			parts := strings.Split(string(firstLine), " ")
			if len(parts) >= 2 {
				target := parts[1]
				if tobj, ok := STORE.Get(target); ok && tobj.Type == "commit" {
					if _, inc := included[target]; !inc && !excluded[target] {
						included[target] = 0
						includedOrder = append(includedOrder, target)
					}
				}
			}
		case "tree":
			for _, sub := range treeClosure(oid) {
				add(sub)
			}
		case "blob":
			add(oid)
		}
	}
	for _, oid := range includedOrder {
		add(oid)
	}
	for _, oid := range includedOrder {
		if t := commitTreeOid(oid); t != "" {
			for _, sub := range treeClosure(t) {
				add(sub)
			}
		}
	}
	if depth == nil && len(haves) == 0 {
		for oid, obj := range STORE.All() {
			if obj.Type != "tag" {
				continue
			}
			firstLine := obj.Data
			if idx := bytes.IndexByte(firstLine, '\n'); idx >= 0 {
				firstLine = firstLine[:idx]
			}
			if !bytes.HasPrefix(firstLine, []byte("object ")) {
				continue
			}
			target := string(firstLine[7:])
			if _, inc := included[target]; inc {
				add(oid)
			}
		}
	}
	var boundary []string
	if depth != nil || len(shallowRoots) > 0 || since != nil {
		for _, oid := range includedOrder {
			obj, _ := STORE.Get(oid)
			for _, parent := range parseCommit(obj.Data).Parents {
				if _, inc := included[parent]; !inc && STORE.Has(parent) {
					boundary = append(boundary, oid)
					break
				}
			}
		}
	}
	return objects, boundary
}
