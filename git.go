package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Git smart HTTP (upload-pack v0/v2, receive-pack v0)
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	return append([]byte(fmt.Sprintf("%04x", len(payload)+4)), payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pktItem struct {
	kind    string // data, flush, delim, response-end, error
	payload []byte
	total   int // bytes consumed including the 4-byte header
}

func iterPktLines(data []byte) []pktItem {
	var items []pktItem
	offset := 0
	for offset+4 <= len(data) {
		length, err := strconv.ParseUint(string(data[offset:offset+4]), 16, 32)
		if err != nil {
			return items
		}
		switch length {
		case 0:
			items = append(items, pktItem{kind: "flush", total: 4})
			offset += 4
		case 1:
			items = append(items, pktItem{kind: "delim", total: 4})
			offset += 4
		case 2:
			items = append(items, pktItem{kind: "response-end", total: 4})
			offset += 4
		case 3:
			items = append(items, pktItem{kind: "error", total: 4})
			offset += 4
		default:
			end := offset + int(length)
			if end > len(data) {
				end = len(data)
			}
			items = append(items, pktItem{kind: "data", payload: data[offset+4 : end], total: int(length)})
			offset += int(length)
		}
	}
	return items
}

func sortedRefItems(repo *Repo) [][2]string {
	refs := make([]string, 0, len(repo.Refs))
	for ref := range repo.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([][2]string, len(refs))
	for i, ref := range refs {
		out[i] = [2]string{ref, repo.Refs[ref]}
	}
	return out
}

func infoRefsReply(repo *Repo, service string, version2 bool) Reply {
	if service == "git-upload-pack" && version2 {
		var body bytes.Buffer
		body.Write(pktLine([]byte("# service=git-upload-pack\n")))
		body.Write(pktFlush)
		body.Write(pktLine([]byte("version 2\n")))
		body.Write(pktLine([]byte("agent=forge/" + FORGE_VERSION + "\n")))
		body.Write(pktLine([]byte("ls-refs=unborn\n")))
		body.Write(pktLine([]byte("fetch=shallow wait-for-done\n")))
		body.Write(pktLine([]byte("server-option\n")))
		body.Write(pktLine([]byte("object-format=sha1\n")))
		body.Write(pktFlush)
		return Reply{Status: 200, Body: body.Bytes(), ContentType: "application/x-git-upload-pack-advertisement"}
	}
	var capabilities string
	if service == "git-upload-pack" {
		capabilities = "multi_ack_detailed no-done side-band-64k thin-pack ofs-delta " +
			"deepen-since deepen-not symref=HEAD:refs/heads/main agent=forge/" + FORGE_VERSION
	} else {
		capabilities = "report-status side-band-64k delete-refs ofs-delta agent=forge/" + FORGE_VERSION
	}
	var body bytes.Buffer
	body.Write(pktLine([]byte("# service=" + service + "\n")))
	body.Write(pktFlush)
	refs := sortedRefItems(repo)
	headOid := repo.Refs["refs/heads/main"]
	if len(refs) > 0 {
		first := true
		if headOid != "" {
			body.Write(pktLine([]byte(fmt.Sprintf("%s HEAD\x00%s\n", headOid, capabilities))))
			first = false
		}
		for _, item := range refs {
			ref, oid := item[0], item[1]
			suffix := ""
			if first {
				suffix = "\x00" + capabilities
			}
			body.Write(pktLine([]byte(fmt.Sprintf("%s %s%s\n", oid, ref, suffix))))
			first = false
			if strings.HasPrefix(ref, "refs/tags/") {
				if obj, ok := STORE.Get(oid); ok && obj.Type == "tag" {
					if target := parseTagTarget(obj.Data); target != "" {
						body.Write(pktLine([]byte(fmt.Sprintf("%s %s^{}\n", target, ref))))
					}
				}
			}
		}
	} else {
		body.Write(pktLine([]byte(fmt.Sprintf("%s capabilities^{}\x00%s\n", ZERO_OID, capabilities))))
	}
	body.Write(pktFlush)
	return Reply{Status: 200, Body: body.Bytes(), ContentType: "application/x-" + service + "-advertisement"}
}

func parseTagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

func sidebandPackets(pack []byte) []byte {
	var out bytes.Buffer
	step := 32768
	for i := 0; i < len(pack); i += step {
		end := i + step
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 0, end-i+1)
		chunk = append(chunk, 0x01)
		chunk = append(chunk, pack[i:end]...)
		out.Write(pktLine(chunk))
	}
	return out.Bytes()
}

func lsRefsReply(repo *Repo, args []string) Reply {
	var prefixes []string
	wantPeel, wantSymrefs, wantUnborn := false, false, false
	for _, a := range args {
		if strings.HasPrefix(a, "ref-prefix ") {
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
		if a == "peel" {
			wantPeel = true
		}
		if a == "symrefs" {
			wantSymrefs = true
		}
		if a == "unborn" {
			wantUnborn = true
		}
	}
	prefixOk := func(ref string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(ref, p) {
				return true
			}
		}
		return false
	}
	var lines []string
	headOid := repo.Refs["refs/heads/main"]
	if wantSymrefs || wantUnborn {
		if prefixOk("HEAD") {
			if headOid != "" {
				lines = append(lines, fmt.Sprintf("%s HEAD symref-target:refs/heads/main", headOid))
			} else if wantUnborn {
				lines = append(lines, "unborn HEAD symref-target:refs/heads/main")
			}
		}
	}
	for _, item := range sortedRefItems(repo) {
		ref, oid := item[0], item[1]
		if !prefixOk(ref) {
			continue
		}
		line := fmt.Sprintf("%s %s", oid, ref)
		if wantPeel && strings.HasPrefix(ref, "refs/tags/") {
			if obj, ok := STORE.Get(oid); ok && obj.Type == "tag" {
				if target := parseTagTarget(obj.Data); target != "" {
					line += " peeled:" + target
				}
			}
		}
		lines = append(lines, line)
	}
	var body bytes.Buffer
	for _, line := range lines {
		body.Write(pktLine([]byte(line + "\n")))
	}
	body.Write(pktFlush)
	return Reply{Status: 200, Body: body.Bytes(), ContentType: "application/x-git-upload-pack-result"}
}

func fetchCacheKey(repo *Repo, wants []string, haves map[string]bool, shallow map[string]bool, depth *int, since *int64) string {
	sortedWants := append([]string{}, wants...)
	sort.Strings(sortedWants)
	var haveList, shallowList []string
	for h := range haves {
		haveList = append(haveList, h)
	}
	for s := range shallow {
		shallowList = append(shallowList, s)
	}
	sort.Strings(haveList)
	sort.Strings(shallowList)
	depthStr := "nil"
	if depth != nil {
		depthStr = strconv.Itoa(*depth)
	}
	sinceStr := "nil"
	if since != nil {
		sinceStr = strconv.FormatInt(*since, 10)
	}
	return repo.Name + "|" + strings.Join(sortedWants, ",") + "|" + strings.Join(haveList, ",") +
		"|" + strings.Join(shallowList, ",") + "|" + depthStr + "|" + sinceStr
}

func fetchReply(repo *Repo, wants []string, haves []string, shallow []string, depth *int, since *int64, version2 bool) Reply {
	haveSet := map[string]bool{}
	for _, h := range haves {
		haveSet[h] = true
	}
	shallowSet := map[string]bool{}
	for _, s := range shallow {
		shallowSet[s] = true
	}
	var common []string
	for _, h := range haves {
		if obj, ok := STORE.Get(h); ok && obj.Type == "commit" {
			common = append(common, h)
		}
	}
	key := fetchCacheKey(repo, wants, haveSet, shallowSet, depth, since)
	var pack []byte
	var framed []byte
	var hasPack bool
	var newShallow map[string]bool
	if cached, ok := FORGE.PackCache[key]; ok {
		pack, framed, hasPack, newShallow = cached.pack, cached.framed, cached.hasPack, cached.newShallow
	} else {
		objects, boundary := packForFetch(wants, haveSet, shallowSet, depth, since)
		if len(objects) > 0 {
			pack = writePack(objects)
			framed = sidebandPackets(pack)
			hasPack = true
		}
		newShallow = map[string]bool{}
		for _, oid := range boundary {
			newShallow[oid] = true
		}
		FORGE.PackCache[key] = &packCacheEntry{pack: pack, framed: framed, hasPack: hasPack, newShallow: newShallow}
	}
	if version2 {
		if !hasPack {
			var body bytes.Buffer
			body.Write(pktLine([]byte("acknowledgments\n")))
			body.Write(pktLine([]byte("NAK\n")))
			body.Write(pktFlush)
			return Reply{Status: 200, Body: body.Bytes(), ContentType: "application/x-git-upload-pack-result"}
		}
		var body bytes.Buffer
		if len(newShallow) > 0 {
			body.Write(pktLine([]byte("shallow-info\n")))
			var shallowList []string
			for oid := range newShallow {
				shallowList = append(shallowList, oid)
			}
			sort.Strings(shallowList)
			for _, oid := range shallowList {
				body.Write(pktLine([]byte(fmt.Sprintf("shallow %s\n", oid))))
			}
			var unshallowList []string
			for oid := range shallowSet {
				if !newShallow[oid] {
					unshallowList = append(unshallowList, oid)
				}
			}
			sort.Strings(unshallowList)
			for _, oid := range unshallowList {
				body.Write(pktLine([]byte(fmt.Sprintf("unshallow %s\n", oid))))
			}
			body.Write(pktFlush)
		}
		body.Write(pktLine([]byte("packfile\n")))
		body.Write(framed)
		body.Write(pktFlush)
		return Reply{Status: 200, Body: body.Bytes(), ContentType: "application/x-git-upload-pack-result"}
	}
	var body bytes.Buffer
	var shallowList []string
	for oid := range newShallow {
		shallowList = append(shallowList, oid)
	}
	sort.Strings(shallowList)
	for _, oid := range shallowList {
		body.Write(pktLine([]byte(fmt.Sprintf("shallow %s\n", oid))))
	}
	var unshallowList []string
	for oid := range shallowSet {
		if !newShallow[oid] {
			unshallowList = append(unshallowList, oid)
		}
	}
	sort.Strings(unshallowList)
	for _, oid := range unshallowList {
		body.Write(pktLine([]byte(fmt.Sprintf("unshallow %s\n", oid))))
	}
	for _, oid := range common {
		body.Write(pktLine([]byte(fmt.Sprintf("ACK %s common\n", oid))))
	}
	if len(common) > 0 {
		body.Write(pktLine([]byte(fmt.Sprintf("ACK %s\n", common[len(common)-1]))))
	} else {
		body.Write(pktLine([]byte("NAK\n")))
	}
	if hasPack {
		body.Write(framed)
	}
	body.Write(pktFlush)
	return Reply{Status: 200, Body: body.Bytes(), ContentType: "application/x-git-upload-pack-result"}
}

func uploadPackReply(repo *Repo, body []byte, version2 bool) Reply {
	if version2 {
		command := ""
		var args []string
		phase := "command"
		for _, item := range iterPktLines(body) {
			if item.kind == "delim" {
				phase = "args"
				continue
			}
			if item.kind == "flush" {
				break
			}
			if item.kind != "data" {
				continue
			}
			line := strings.TrimSuffix(string(item.payload), "\n")
			if phase == "command" {
				if strings.HasPrefix(line, "command=") {
					command = line[len("command="):]
				}
			} else {
				args = append(args, line)
			}
		}
		if command == "ls-refs" {
			return lsRefsReply(repo, args)
		}
		if command == "fetch" {
			var wants, haves, shallow []string
			var depth *int
			var since *int64
			done := false
			for _, line := range args {
				switch {
				case strings.HasPrefix(line, "want "):
					wants = append(wants, strings.TrimSpace(line[5:]))
				case strings.HasPrefix(line, "have "):
					haves = append(haves, strings.TrimSpace(line[5:]))
				case strings.HasPrefix(line, "shallow "):
					shallow = append(shallow, strings.TrimSpace(line[8:]))
				case strings.HasPrefix(line, "deepen-since "):
					if n, err := strconv.ParseInt(strings.TrimSpace(line[13:]), 10, 64); err == nil {
						since = &n
					}
				case strings.HasPrefix(line, "deepen "):
					if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
						depth = &n
					}
				case line == "done":
					done = true
				}
			}
			if !done {
				var out bytes.Buffer
				out.Write(pktLine([]byte("acknowledgments\n")))
				out.Write(pktLine([]byte("NAK\n")))
				out.Write(pktFlush)
				return Reply{Status: 200, Body: out.Bytes(), ContentType: "application/x-git-upload-pack-result"}
			}
			return fetchReply(repo, wants, haves, shallow, depth, since, true)
		}
		return Reply{Status: 400, Body: []byte("unsupported command"), ContentType: "text/plain"}
	}
	var wants, haves, shallow []string
	var depth *int
	var since *int64
	for _, item := range iterPktLines(body) {
		if item.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(item.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			parts := strings.SplitN(line[5:], " ", 2)
			wants = append(wants, parts[0])
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen-since "):
			if n, err := strconv.ParseInt(strings.TrimSpace(line[13:]), 10, 64); err == nil {
				since = &n
			}
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				depth = &n
			}
		}
	}
	return fetchReply(repo, wants, haves, shallow, depth, since, false)
}

func receivePackReply(repo *Repo, body []byte) Reply {
	type refCommand struct{ old, new, ref string }
	var commands []refCommand
	capabilities := ""
	offset := 0
	for _, item := range iterPktLines(body) {
		offset += item.total
		if item.kind == "flush" {
			break
		}
		if item.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(item.payload), "\n")
		if idx := strings.Index(line, "\x00"); idx >= 0 {
			capabilities = line[idx+1:]
			line = line[:idx]
		}
		parts := strings.Split(line, " ")
		if len(parts) == 3 {
			commands = append(commands, refCommand{parts[0], parts[1], parts[2]})
		}
	}
	var pack []byte
	if offset < len(body) {
		pack = body[offset:]
	}
	unpackOk := true
	if len(pack) > 0 {
		if _, err := readPack(pack); err != nil {
			unpackOk = false
		}
	}
	var results []string
	for _, cmd := range commands {
		reason := ""
		if !strings.HasPrefix(cmd.ref, "refs/heads/") && !strings.HasPrefix(cmd.ref, "refs/tags/") {
			reason = "invalid ref"
		} else {
			current := repo.Refs[cmd.ref]
			if current == "" {
				current = ZERO_OID
			}
			if cmd.old != current {
				reason = "stale info"
			} else if cmd.new != ZERO_OID {
				if !STORE.Has(cmd.new) {
					reason = "missing object"
				} else if strings.HasPrefix(cmd.ref, "refs/heads/") {
					if STORE.TypeOf(cmd.new) != "commit" {
						reason = "not a commit"
					} else if cmd.old != ZERO_OID {
						if !isAncestor(cmd.new, cmd.old) {
							reason = "non-fast-forward"
						}
					}
				}
			}
			if reason == "" && !unpackOk && cmd.new != ZERO_OID {
				reason = "unpack failed"
			}
		}
		if reason == "" {
			if cmd.new == ZERO_OID {
				delete(repo.Refs, cmd.ref)
			} else {
				repo.Refs[cmd.ref] = cmd.new
			}
			results = append(results, "ok "+cmd.ref)
		} else {
			results = append(results, fmt.Sprintf("ng %s %s", cmd.ref, reason))
		}
	}
	FORGE.PackCache = map[string]*packCacheEntry{}
	reportLines := []string{"unpack ok"}
	if !unpackOk {
		reportLines = []string{"unpack error"}
	}
	reportLines = append(reportLines, results...)
	var report strings.Builder
	for _, line := range reportLines {
		report.WriteString(line + "\n")
	}
	reportBytes := []byte(report.String())
	var payload bytes.Buffer
	if strings.Contains(capabilities, "side-band-64k") {
		payload.Write(sidebandPackets(reportBytes))
		payload.Write(pktFlush)
	} else {
		if len(reportBytes) > 0 {
			for _, line := range strings.SplitAfter(report.String(), "\n") {
				if line == "" {
					continue
				}
				payload.Write(pktLine([]byte(line)))
			}
		}
		payload.Write(pktFlush)
	}
	return Reply{Status: 200, Body: payload.Bytes(), ContentType: "application/x-git-receive-pack-result"}
}
