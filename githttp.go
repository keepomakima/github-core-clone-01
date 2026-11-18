// Git smart-HTTP: info/refs, upload-pack (protocol v2 and v0) and
// receive-pack (v0). The exact wire shapes the conformance suite's git
// client speaks, documented in the kit: pkt-line framing, v2 capability
// advertisement, ls-refs/fetch commands, shallow/deepen handling, sideband
// packfiles, report-status. Standard library only.

package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const uploadV0Caps = "multi_ack thin-pack side-band-64k side-band ofs-delta shallow symref=HEAD:refs/heads/main agent=forge/1.0"
const receiveV0Caps = "report-status side-band-64k delete-refs ofs-delta symref=HEAD:refs/heads/main agent=forge/1.0"

type cachedAdvertisements struct {
	version    int
	v0Upload   []byte
	v2Upload   []byte
	v0Receive  []byte
}

type gitResult struct {
	status      int
	contentType string
	body        []byte
}

func jsonErrorResult(status int, message string) gitResult {
	return gitResult{status: status, contentType: "application/json", body: marshalJSON(map[string]any{"message": message})}
}

func cachedResponse(r *repo, key string, build func() []byte) []byte {
	if r.responseCache == nil {
		r.responseCache = map[string][]byte{}
	}
	if hit, ok := r.responseCache[key]; ok {
		return hit
	}
	built := build()
	if len(r.responseCache) > 64 {
		r.responseCache = map[string][]byte{}
	}
	r.responseCache[key] = built
	return built
}

func sortedRefs(r *repo) []struct {
	name string
	oid  string
} {
	var refs []struct {
		name string
		oid  string
	}
	for name, oid := range r.refs {
		refs = append(refs, struct {
			name string
			oid  string
		}{name, oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

func peeledOid(r *repo, ref, oid string) string {
	if !strings.HasPrefix(ref, "refs/tags/") {
		return ""
	}
	object := r.objects[oid]
	if object == nil || object.typ != objTag {
		return ""
	}
	target := parseTag(object.data).object
	if isObjectID(target) {
		return target
	}
	return ""
}

func v0RefLines(r *repo, caps string) []string {
	refs := sortedRefs(r)
	head, hasHead := r.refs["refs/heads/"+r.defaultBranch]
	if len(refs) == 0 {
		return []string{zeroOID + " capabilities^{}\x00" + caps}
	}
	headOid := head
	if !hasHead {
		headOid = zeroOID
	}
	lines := []string{headOid + " HEAD\x00" + caps}
	for _, ref := range refs {
		lines = append(lines, ref.oid+" "+ref.name)
		if peeled := peeledOid(r, ref.name, ref.oid); peeled != "" {
			lines = append(lines, peeled+" "+ref.name+"^{}")
		}
	}
	return lines
}

func advertisements(r *repo) *cachedAdvertisements {
	if r.advCache != nil && r.advCache.version == r.version {
		return r.advCache
	}
	v0Lines := append([]pktItem{pktData("# service=git-upload-pack\n"), pktFlushItem()}, strItems(v0RefLines(r, uploadV0Caps))...)
	v2Lines := []string{"version 2\n", "agent=forge/1.0\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n"}
	rLines := append([]pktItem{pktData("# service=git-receive-pack\n"), pktFlushItem()}, strItems(v0RefLines(r, receiveV0Caps))...)
	built := &cachedAdvertisements{
		version:   r.version,
		v0Upload:  pktLineItems(append(v0Lines, pktFlushItem())),
		v2Upload:  pktLineItems(append(strItems(v2Lines), pktFlushItem())),
		v0Receive: pktLineItems(append(rLines, pktFlushItem())),
	}
	r.advCache = built
	return built
}

func strItems(lines []string) []pktItem {
	items := make([]pktItem, 0, len(lines))
	for _, l := range lines {
		items = append(items, pktData(l))
	}
	return items
}

// sidebandPackets splits a sideband pack into pkt-lines (band 1, at most
// 65515 data bytes each).
func sidebandPackets(pack []byte) [][]byte {
	var out [][]byte
	for offset := 0; offset < len(pack); offset += 65515 {
		end := offset + 65515
		if end > len(pack) {
			end = len(pack)
		}
		chunk := pack[offset:end]
		framed := make([]byte, 0, len(chunk)+1)
		framed = append(framed, 1)
		framed = append(framed, chunk...)
		out = append(out, pktLine(framed))
	}
	return out
}

type fetchPlan struct {
	objects   []*gitObject
	shallow   []string
	unshallow []string
}

// planFetch decides what a fetch sends: reachable(wants) minus
// reachable(haves), with a depth limit. Commit distance counts generations
// from the nearest want; the boundary generation is listed as shallow.
// oldBoundaries are the shallow tips the client already holds — any it now
// fully receives becomes unshallow.
func planFetch(r *repo, wants, haves []string, depth *int, depthRelative bool, oldBoundaries []string) fetchPlan {
	// "deepen N" with "deepen-relative" (protocol v2) deepens from the
	// client's current shallow depth, not from the tip: the current depth
	// is the distance from a want to its shallow boundary plus one. Without
	// the flag the value is an absolute depth. v4 treated every deepen as
	// absolute, so a real git client's --deepen fetched nothing new.
	if depth != nil && depthRelative {
		if current := currentShallowDepth(r, wants, oldBoundaries); current > 0 {
			eff := current + *depth
			depth = &eff
		}
	}
	lookup := func(oid string) *gitObject { return r.objects[oid] }
	distance := map[string]int{}
	type qitem struct {
		oid   string
		depth int
	}
	var queue []qitem
	queueHead := 0
	for _, want := range wants {
		object := lookup(want)
		if object == nil {
			continue
		}
		if object.typ == objTag {
			target := parseTag(object.data).object
			if isObjectID(target) {
				queue = append(queue, qitem{oid: target, depth: 0})
			}
		} else {
			queue = append(queue, qitem{oid: want, depth: 0})
		}
	}
	includedCommits := map[string]bool{}
	for queueHead < len(queue) {
		item := queue[queueHead]
		queueHead++
		oid, d := item.oid, item.depth
		if _, ok := distance[oid]; ok {
			continue
		}
		object := lookup(oid)
		if object == nil {
			continue
		}
		distance[oid] = d
		if depth != nil && d >= *depth {
			continue
		}
		includedCommits[oid] = true
		if object.typ == objCommit {
			for _, parent := range cachedParseCommit(object.data).parents {
				queue = append(queue, qitem{oid: parent, depth: d + 1})
			}
		}
	}
	var shallow []string
	if depth != nil {
		for oid, d := range distance {
			if d == *depth-1 && includedCommits[oid] {
				shallow = append(shallow, oid)
			}
		}
	}
	shallowSet := map[string]bool{}
	for _, oid := range shallow {
		shallowSet[oid] = true
	}
	var unshallow []string
	for _, boundary := range oldBoundaries {
		if includedCommits[boundary] && !shallowSet[boundary] {
			unshallow = append(unshallow, boundary)
		}
	}
	// Objects the client already holds. A have that is one of the client's
	// shallow boundaries counts only for itself and its tree: a shallow clone
	// holds the boundary commit but none of its parents, so the walk must not
	// cross a boundary or a deepen would exclude the very parent it must send.
	oldBoundarySet := map[string]bool{}
	for _, b := range oldBoundaries {
		oldBoundarySet[b] = true
	}
	held := map[string]bool{}
	{
		stack := append([]string{}, haves...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if held[oid] {
				continue
			}
			object := lookup(oid)
			if object == nil {
				continue
			}
			held[oid] = true
			switch object.typ {
			case objCommit:
				commit := cachedParseCommit(object.data)
				stack = append(stack, commit.tree)
				if !oldBoundarySet[oid] {
					stack = append(stack, commit.parents...)
				}
			case objTag:
				stack = append(stack, parseTag(object.data).object)
			case objTree:
				for _, entry := range cachedParseTree(object.data) {
					if entry.mode != "160000" {
						stack = append(stack, entry.oid)
					}
				}
			}
		}
	}
	// Walk the included commits without crossing the depth boundary: a commit
	// contributes its tree and blobs, and its parents only when they are
	// themselves included. (collectReachable would walk the whole history.)
	wanted := map[string]bool{}
	{
		stack := append([]string{}, wants...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if wanted[oid] {
				continue
			}
			object := lookup(oid)
			if object == nil {
				continue
			}
			wanted[oid] = true
			switch object.typ {
			case objCommit:
				commit := cachedParseCommit(object.data)
				stack = append(stack, commit.tree)
				for _, parent := range commit.parents {
					if includedCommits[parent] {
						stack = append(stack, parent)
					}
				}
			case objTag:
				stack = append(stack, parseTag(object.data).object)
			case objTree:
				for _, entry := range cachedParseTree(object.data) {
					if entry.mode != "160000" {
						stack = append(stack, entry.oid)
					}
				}
			}
		}
	}
	// Deterministic pack order (the TS version used Set insertion order; the
	// client reads objects by oid, so order is free — sort for stability).
	// The wanted map is already keyed by oid, so sort by the key directly
	// instead of re-hashing every object per comparison (the old code did
	// O(n log n) SHA-1 hashes here).
	type oidObject struct {
		oid string
		obj *gitObject
	}
	paired := make([]oidObject, 0, len(wanted))
	for oid := range wanted {
		if held[oid] {
			continue
		}
		if object := lookup(oid); object != nil {
			paired = append(paired, oidObject{oid: oid, obj: object})
		}
	}
	sort.Slice(paired, func(i, j int) bool { return paired[i].oid < paired[j].oid })
	objects := make([]*gitObject, 0, len(paired))
	for _, p := range paired {
		objects = append(objects, p.obj)
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)
	return fetchPlan{objects: objects, shallow: shallow, unshallow: unshallow}
}

func packFor(r *repo, key string, build func() []byte) []byte {
	if r.packCache == nil {
		r.packCache = map[string][]byte{}
	}
	if hit, ok := r.packCache[key]; ok {
		return hit
	}
	built := build()
	if len(r.packCache) > 64 {
		r.packCache = map[string][]byte{}
	}
	r.packCache[key] = built
	return built
}

// ---------------------------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------------------------

func handleInfoRefs(r *repo, service string, protocolVersion string) gitResult {
	ads := advertisements(r)
	if service == "git-upload-pack" {
		if protocolVersion == "2" {
			return gitResult{status: 200, contentType: "application/x-git-upload-pack-advertisement", body: ads.v2Upload}
		}
		return gitResult{status: 200, contentType: "application/x-git-upload-pack-advertisement", body: ads.v0Upload}
	}
	if service == "git-receive-pack" {
		return gitResult{status: 200, contentType: "application/x-git-receive-pack-advertisement", body: ads.v0Receive}
	}
	return jsonErrorResult(404, "Not Found")
}

// ---------------------------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------------------------

type lsRefsOpts struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

// currentShallowDepth returns the client's current shallow depth: the
// distance from the nearest want to any of its shallow boundaries, plus one
// (the tip itself is depth 1). Zero when no boundary is reachable.
func currentShallowDepth(r *repo, wants, oldBoundaries []string) int {
	if len(oldBoundaries) == 0 {
		return 0
	}
	boundarySet := map[string]bool{}
	for _, b := range oldBoundaries {
		boundarySet[b] = true
	}
	type qitem struct {
		oid   string
		depth int
	}
	seen := map[string]bool{}
	queue := []qitem{}
	for _, want := range wants {
		if object := r.objects[want]; object != nil {
			queue = append(queue, qitem{oid: want, depth: 0})
		}
	}
	for head := 0; head < len(queue); head++ {
		item := queue[head]
		if seen[item.oid] {
			continue
		}
		seen[item.oid] = true
		if boundarySet[item.oid] {
			return item.depth + 1
		}
		object := r.objects[item.oid]
		if object == nil || object.typ != objCommit {
			continue
		}
		for _, parent := range cachedParseCommit(object.data).parents {
			queue = append(queue, qitem{oid: parent, depth: item.depth + 1})
		}
	}
	return 0
}

func lsRefsResponse(r *repo, opts lsRefsOpts) []byte {
	var lines []string
	head, hasHead := r.refs["refs/heads/"+r.defaultBranch]
	matchPrefix := func(name string) bool {
		if len(opts.prefixes) == 0 {
			return true
		}
		for _, p := range opts.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	refs := sortedRefs(r)
	if len(refs) == 0 {
		if opts.unborn {
			lines = append(lines, "unborn HEAD symref-target:refs/heads/main")
		}
		return pktLineItems(append(strItems(lines), pktFlushItem()))
	}
	if matchPrefix("HEAD") && hasHead {
		if opts.symrefs {
			lines = append(lines, head+" HEAD symref-target:refs/heads/main")
		} else {
			lines = append(lines, head+" HEAD")
		}
	}
	for _, ref := range refs {
		if !matchPrefix(ref.name) {
			continue
		}
		line := ref.oid + " " + ref.name
		if opts.peel {
			if peeled := peeledOid(r, ref.name, ref.oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		lines = append(lines, line)
	}
	return pktLineItems(append(strItems(lines), pktFlushItem()))
}

func ackLines(r *repo, haves []string) []string {
	for _, have := range haves {
		if _, ok := r.objects[have]; ok {
			return []string{"ACK " + have + "\n"}
		}
	}
	return []string{"NAK\n"}
}

func dataLines(packets []pkt) []string {
	var lines []string
	for _, p := range packets {
		if p.kind == "data" {
			lines = append(lines, string(p.payload))
		}
	}
	return lines
}

func v2UploadPack(r *repo, body []byte) gitResult {
	packets, _ := decodePktLines(body)
	lines := dataLines(packets)
	commandLine := ""
	if len(lines) > 0 {
		commandLine = lines[0]
	}
	command := ""
	if strings.HasPrefix(commandLine, "command=") {
		command = strings.TrimSpace(commandLine[8:])
	}
	if command == "ls-refs" {
		opts := lsRefsOpts{}
		for _, line := range lines[1:] {
			text := strings.TrimSpace(line)
			switch {
			case text == "peel":
				opts.peel = true
			case text == "symrefs":
				opts.symrefs = true
			case text == "unborn":
				opts.unborn = true
			case strings.HasPrefix(text, "ref-prefix "):
				opts.prefixes = append(opts.prefixes, text[len("ref-prefix "):])
			}
		}
		key := strconv.Itoa(r.version) + ":ls-refs:" + boolStr(opts.peel) + ":" + boolStr(opts.symrefs) + ":" + boolStr(opts.unborn) + ":" + strings.Join(opts.prefixes, ",")
		responseBody := cachedResponse(r, key, func() []byte { return lsRefsResponse(r, opts) })
		return gitResult{status: 200, contentType: "application/x-git-upload-pack-result", body: responseBody}
	}
	if command == "fetch" {
		var wants, haves, oldBoundaries []string
		var depth *int
		deepenRelative := false
		for _, line := range lines[1:] {
			text := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(text, "want "):
				wants = append(wants, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "have "):
				haves = append(haves, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "shallow "):
				oldBoundaries = append(oldBoundaries, strings.TrimSpace(text[8:]))
			case text == "deepen-relative":
				deepenRelative = true
			case strings.HasPrefix(text, "deepen "):
				if parsed, err := strconv.Atoi(strings.TrimSpace(text[7:])); err == nil {
					depth = &parsed
				}
			}
		}
		depthStr := ""
		if depth != nil {
			depthStr = strconv.Itoa(*depth)
		}
		fetchKey := strconv.Itoa(r.version) + ":v2fetch:" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + depthStr + "|" + boolStr(deepenRelative) + "|" + strings.Join(oldBoundaries, ",")
		responseBody := cachedResponse(r, fetchKey, func() []byte {
			plan := planFetch(r, wants, haves, depth, deepenRelative, oldBoundaries)
			var parts [][]byte
			// The acknowledgments section only exists when the client sent
			// "have" lines: with no haves there is nothing to negotiate and
			// a strict client expects the response to start with the
			// packfile (or shallow-info) section instead.
			hasSections := len(plan.shallow) > 0 || len(plan.unshallow) > 0 || len(plan.objects) > 0
			if len(haves) > 0 {
				parts = append(parts, pktLineStr("acknowledgments\n"))
				acks := ackLines(r, haves)
				for _, line := range acks {
					parts = append(parts, pktLineStr(line))
				}
				if len(acks) > 0 && acks[0] != "NAK\n" {
					parts = append(parts, pktLineStr("ready\n"))
				}
				if hasSections {
					parts = append(parts, pktDelim)
				}
			}
			if hasSections {
				if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
					parts = append(parts, pktLineStr("shallow-info\n"))
					for _, oid := range plan.shallow {
						parts = append(parts, pktLineStr("shallow "+oid+"\n"))
					}
					for _, oid := range plan.unshallow {
						parts = append(parts, pktLineStr("unshallow "+oid+"\n"))
					}
					parts = append(parts, pktDelim)
				}
				if len(plan.objects) > 0 {
					parts = append(parts, pktLineStr("packfile\n"))
					key := strconv.Itoa(r.version) + ":v2:" + strings.Join(wants, ",") + ":" + strings.Join(haves, ",") + ":" + depthStr
					parts = append(parts, sidebandPackets(packFor(r, key, func() []byte { return writePack(plan.objects) }))...)
				}
			}
			parts = append(parts, pktFlush)
			return bytes.Join(parts, nil)
		})
		return gitResult{status: 200, contentType: "application/x-git-upload-pack-result", body: responseBody}
	}
	return jsonErrorResult(400, "unknown command")
}

func v0UploadPack(r *repo, body []byte) gitResult {
	packets, _ := decodePktLines(body)
	dataLinesList := dataLines(packets)
	var wants, haves, oldBoundaries []string
	var depth *int
	seenFlush := false
	for _, packet := range packets {
		if packet.kind == "flush" {
			seenFlush = true
			continue
		}
		if packet.kind != "data" {
			continue
		}
		text := string(packet.payload)
		if !seenFlush {
			if strings.HasPrefix(text, "want ") {
				rest := strings.TrimSpace(text[5:])
				wants = append(wants, strings.Split(rest, " ")[0])
			} else if strings.HasPrefix(text, "shallow ") {
				oldBoundaries = append(oldBoundaries, strings.TrimSpace(text[8:]))
			} else if strings.HasPrefix(text, "deepen ") {
				if parsed, err := strconv.Atoi(strings.TrimSpace(text[7:])); err == nil {
					depth = &parsed
				}
			}
		} else if strings.HasPrefix(text, "have ") {
			haves = append(haves, strings.TrimSpace(text[5:]))
		}
	}
	if len(wants) == 0 || len(dataLinesList) == 0 {
		return jsonErrorResult(400, "bad request")
	}
	depthStr := ""
	if depth != nil {
		depthStr = strconv.Itoa(*depth)
	}
	fetchKey := strconv.Itoa(r.version) + ":v0fetch:" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + depthStr + "|" + strings.Join(oldBoundaries, ",")
	responseBody := cachedResponse(r, fetchKey, func() []byte {
		plan := planFetch(r, wants, haves, depth, false, oldBoundaries)
		var parts [][]byte
		for _, line := range ackLines(r, haves) {
			parts = append(parts, pktLineStr(line))
		}
		for _, oid := range plan.shallow {
			parts = append(parts, pktLineStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			parts = append(parts, pktLineStr("unshallow "+oid+"\n"))
		}
		if len(plan.objects) > 0 {
			key := strconv.Itoa(r.version) + ":v0:" + strings.Join(wants, ",") + ":" + strings.Join(haves, ",") + ":" + depthStr
			parts = append(parts, sidebandPackets(packFor(r, key, func() []byte { return writePack(plan.objects) }))...)
		}
		parts = append(parts, pktFlush)
		return bytes.Join(parts, nil)
	})
	return gitResult{status: 200, contentType: "application/x-git-upload-pack-result", body: responseBody}
}

func handleUploadPack(r *repo, body []byte, protocolVersion string) gitResult {
	if !startsWithPktLine(body) {
		return jsonErrorResult(400, "bad request")
	}
	if protocolVersion == "2" {
		return v2UploadPack(r, body)
	}
	return v0UploadPack(r, body)
}

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

type refCommand struct {
	oldOid string
	newOid string
	ref    string
}

func handleReceivePack(s *store, r *repo, body []byte) gitResult {
	if !startsWithPktLine(body) {
		return jsonErrorResult(400, "bad request")
	}
	packets, consumed := decodePktLines(body)
	var commands []refCommand
	for _, packet := range packets {
		if packet.kind != "data" {
			continue
		}
		text := string(packet.payload)
		if nul := strings.Index(text, "\x00"); nul >= 0 {
			text = text[:nul]
		}
		parts := strings.Split(strings.TrimSpace(text), " ")
		if len(parts) >= 3 && isObjectID(parts[0]) && isObjectID(parts[1]) {
			commands = append(commands, refCommand{oldOid: parts[0], newOid: parts[1], ref: parts[2]})
		}
	}
	if len(commands) == 0 {
		return jsonErrorResult(400, "bad request")
	}
	packBytes := body[consumed:]
	if len(packBytes) > 0 {
		result, err := readPack(packBytes, func(oid string) *gitObject { return r.objects[oid] })
		if err != nil {
			return jsonErrorResult(400, err.Error())
		}
		for oid, object := range result.objects {
			if _, ok := r.objects[oid]; !ok {
				r.objects[oid] = object
			}
		}
	}
	results := []string{"unpack ok\n"}
	changed := false
	for _, command := range commands {
		current, hasCurrent := r.refs[command.ref]
		isTag := strings.HasPrefix(command.ref, "refs/tags/")
		fail := func(reason string) {
			results = append(results, "ng "+command.ref+" "+reason+"\n")
		}
		if command.newOid == zeroOID {
			if !hasCurrent {
				fail("no such ref")
			} else if command.oldOid != zeroOID && command.oldOid != current {
				fail("non-fast-forward")
			} else {
				delete(r.refs, command.ref)
				results = append(results, "ok "+command.ref+"\n")
				changed = true
			}
			continue
		}
		object := r.objects[command.newOid]
		if object == nil {
			fail("object not found")
			continue
		}
		if command.oldOid == zeroOID {
			if hasCurrent {
				fail("already exists")
			} else {
				r.refs[command.ref] = command.newOid
				results = append(results, "ok "+command.ref+"\n")
				changed = true
			}
			continue
		}
		if !hasCurrent {
			fail("no such ref")
			continue
		}
		if command.oldOid != current {
			fail("non-fast-forward")
			continue
		}
		fastForward := isTag || object.typ != objCommit || s.isAncestorOf(r, command.oldOid, command.newOid)
		if !fastForward {
			fail("non-fast-forward")
			continue
		}
		r.refs[command.ref] = command.newOid
		results = append(results, "ok "+command.ref+"\n")
		changed = true
	}
	if changed {
		s.touch(r)
	}
	var parts [][]byte
	for _, line := range results {
		// The report-status stream is itself pkt-lined inside band 1: the
		// client demultiplexes band 1 and then reads pkt-lines from it, so
		// a raw "unpack ok" there is read as a length header ("bad line
		// length character: unpa"). Frame each line as an inner pkt-line.
		inner := pktLine([]byte(line))
		framed := append([]byte{1}, inner...)
		parts = append(parts, pktLine(framed))
	}
	// The report-status stream inside band 1 ends with its own flush; without
	// it the client reads past the last line and reports a hangup. The outer
	// flush after it ends the sideband stream itself.
	parts = append(parts, pktLine(append([]byte{1}, []byte("0000")...)))
	parts = append(parts, pktFlush)
	return gitResult{status: 200, contentType: "application/x-git-receive-pack-result", body: bytes.Join(parts, nil)}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
