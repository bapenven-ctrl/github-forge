#!/usr/bin/env python3
"""A GitHub-compatible git forge in one process, standard library only.

Surfaces: git smart HTTP (protocol v0 and v2), the REST v3 API, GraphQL v4
and an MCP endpoint over streamable HTTP, all over one in-memory store.
The process binds an ephemeral port, registers it with the trial env at
$MAKEFUN_ENV_URL, polls the env until it says done, and exits 0.
"""

import base64
import difflib
import hashlib
import json
import os
import re
import socket
import sys
import threading
import time
import urllib.request
import zlib
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, unquote, urlparse

VIEWER_LOGIN = "adaiversen"
VIEWER_ID = 1000
VIEWER_NAME = "ada iversen"
VIEWER_EMAIL = "adaiversen@example.invalid"
STRANGER_LOGIN = "olgabauer"
ZERO_OID = "0" * 40
FORGE_VERSION = "1.0.0"
LOCK = threading.RLock()


class LogicalClock:
    """Deterministic timestamps: a fixed base plus one second per event."""

    def __init__(self):
        self.value = 1739175597

    def tick(self):
        self.value += 1
        return self.value


CLOCK = LogicalClock()


def iso_of(ts):
    return datetime.fromtimestamp(ts, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def git_stamp(ts):
    return "%d +0000" % ts


def viewer_ident(ts):
    return "%s <%s> %s" % (VIEWER_NAME, VIEWER_EMAIL, git_stamp(ts))


# --------------------------------------------------------------------------
# git object store
# --------------------------------------------------------------------------

TYPE_CODES = {"commit": 1, "tree": 2, "blob": 3, "tag": 4}
CODE_TYPES = {code: name for name, code in TYPE_CODES.items()}


class ObjectStore:
    def __init__(self):
        self.objects = {}

    def put(self, otype, data):
        oid = hashlib.sha1(("%s %d" % (otype, len(data))).encode() + b"\x00" + data).hexdigest()
        self.objects[oid] = (otype, data)
        return oid

    def get(self, oid):
        return self.objects.get(oid)

    def has(self, oid):
        return oid in self.objects

    def type_of(self, oid):
        found = self.objects.get(oid)
        return found[0] if found else None


STORE = ObjectStore()


def commit_ts(oid):
    found = STORE.get(oid)
    if not found or found[0] != "commit":
        return 0
    committer = parse_commit(found[1])["committer"]
    parts = committer.rsplit(" ", 2)
    try:
        return int(parts[-2])
    except Exception:
        return 0


def parse_commit(data):
    head, _, message = data.partition(b"\n\n")
    parsed = {"tree": None, "parents": [], "author": "", "committer": "", "message": message.decode("utf-8", "replace")}
    for line in head.split(b"\n"):
        if line.startswith(b"tree "):
            parsed["tree"] = line[5:].decode()
        elif line.startswith(b"parent "):
            parsed["parents"].append(line[7:].decode())
        elif line.startswith(b"author "):
            parsed["author"] = line[7:].decode("utf-8", "replace")
        elif line.startswith(b"committer "):
            parsed["committer"] = line[10:].decode("utf-8", "replace")
    return parsed


def build_commit(tree_oid, parents, author, committer, message):
    lines = ["tree %s" % tree_oid]
    for parent in parents:
        lines.append("parent %s" % parent)
    lines.append("author %s" % author)
    lines.append("committer %s" % committer)
    return ("\n".join(lines) + "\n\n" + message).encode()


def parse_tree(data):
    entries = []
    pos = 0
    while pos < len(data):
        space = data.index(b" ", pos)
        mode = data[pos:space].decode()
        nul = data.index(b"\x00", space)
        name = data[space + 1:nul].decode("utf-8", "surrogateescape")
        oid = data[nul + 1:nul + 21].hex()
        entries.append((mode, name, oid))
        pos = nul + 21
    return entries


def build_tree(entries):
    def sort_key(entry):
        mode, name, _ = entry
        return name + "/" if mode == "40000" else name

    out = bytearray()
    for mode, name, oid in sorted(entries, key=sort_key):
        out += mode.encode() + b" " + name.encode("utf-8", "surrogateescape") + b"\x00" + bytes.fromhex(oid)
    return bytes(out)


def flatten_tree(tree_oid, prefix=""):
    flat = {}
    if tree_oid is None:
        return flat
    found = STORE.get(tree_oid)
    if found is None or found[0] != "tree":
        return flat
    for mode, name, oid in parse_tree(found[1]):
        path = prefix + name
        if mode == "40000":
            flat.update(flatten_tree(oid, path + "/"))
        else:
            flat[path] = (mode, oid)
    return flat


def tree_from_flat(flat):
    nested = {}
    for path, value in flat.items():
        parts = path.split("/")
        node = nested
        for part in parts[:-1]:
            node = node.setdefault(part, {})
        node[parts[-1]] = value

    def emit(node):
        entries = []
        for name, value in node.items():
            if isinstance(value, dict):
                entries.append(("40000", name, emit(value)))
            else:
                entries.append((value[0], name, value[1]))
        return STORE.put("tree", build_tree(entries))

    return emit(nested)


def commit_tree_oid(commit_oid):
    found = STORE.get(commit_oid)
    if found is None or found[0] != "commit":
        return None
    return parse_commit(found[1])["tree"]


def commit_flat(commit_oid):
    return flatten_tree(commit_tree_oid(commit_oid))


def ancestors_of(tip):
    """All commit oids reachable from tip, tip first, parents in order."""
    order = []
    seen = set()
    stack = [tip]
    while stack:
        oid = stack.pop()
        if oid in seen or not STORE.has(oid):
            continue
        seen.add(oid)
        order.append(oid)
        parsed = parse_commit(STORE.get(oid)[1])
        for parent in reversed(parsed["parents"]):
            stack.append(parent)
    return order


def merge_base_of(a_oid, b_oid):
    if a_oid is None or b_oid is None:
        return None
    a_ancestors = set(ancestors_of(a_oid))
    queue = [b_oid]
    seen = set()
    while queue:
        oid = queue.pop(0)
        if oid in seen or not STORE.has(oid):
            continue
        seen.add(oid)
        if oid in a_ancestors:
            return oid
        queue.extend(parse_commit(STORE.get(oid)[1])["parents"])
    return None


# --------------------------------------------------------------------------
# packfiles
# --------------------------------------------------------------------------


def write_pack(oids):
    out = bytearray(b"PACK" + (2).to_bytes(4, "big") + len(oids).to_bytes(4, "big"))
    for oid in oids:
        otype, data = STORE.get(oid)
        # encode the variable-length size/type header
        encoded = bytearray()
        remaining = len(data) >> 4
        byte0 = (TYPE_CODES[otype] << 4) | (len(data) & 0x0F)
        if remaining:
            byte0 |= 0x80
        encoded.append(byte0)
        while remaining:
            chunk = remaining & 0x7F
            remaining >>= 7
            if remaining:
                chunk |= 0x80
            encoded.append(chunk)
        out += encoded
        out += zlib.compress(data)
    out += hashlib.sha1(bytes(out)).digest()
    return bytes(out)


def apply_delta(base, delta):
    pos = 0

    def read_varint():
        nonlocal pos
        value = 0
        shift = 0
        while True:
            byte = delta[pos]
            pos += 1
            value |= (byte & 0x7F) << shift
            shift += 7
            if not byte & 0x80:
                return value

    source_size = read_varint()
    target_size = read_varint()
    if source_size != len(base):
        raise ValueError("delta base size mismatch")
    out = bytearray()
    while pos < len(delta):
        op = delta[pos]
        pos += 1
        if op & 0x80:
            offset = 0
            size = 0
            for bit in range(4):
                if op & (1 << bit):
                    offset |= delta[pos] << (8 * bit)
                    pos += 1
            for bit in range(3):
                if op & (0x10 << bit):
                    size |= delta[pos] << (8 * bit)
                    pos += 1
            if size == 0:
                size = 0x10000
            out += base[offset:offset + size]
        elif op:
            out += delta[pos:pos + op]
            pos += op
        else:
            raise ValueError("delta opcode 0")
    if len(out) != target_size:
        raise ValueError("delta target size mismatch")
    return bytes(out)


def read_pack(data):
    if len(data) < 12 or data[:4] != b"PACK":
        raise ValueError("not a packfile")
    count = int.from_bytes(data[8:12], "big")
    pos = 12
    stored = []
    pending = []
    offsets = {}
    for _ in range(count):
        entry_start = pos
        first = data[pos]
        pos += 1
        raw_type = (first >> 4) & 0x07
        otype = CODE_TYPES.get(raw_type, "blob")
        size = first & 0x0F
        shift = 4
        byte = first
        while byte & 0x80:
            byte = data[pos]
            pos += 1
            size |= (byte & 0x7F) << shift
            shift += 7
        base_oid = None
        base_offset = None
        if raw_type == 6:  # OFS_DELTA
            byte = data[pos]
            pos += 1
            offset = byte & 0x7F
            while byte & 0x80:
                byte = data[pos]
                pos += 1
                offset = ((offset + 1) << 7) | (byte & 0x7F)
            base_offset = offset
            otype = "ofs-delta"
        elif raw_type == 7:  # REF_DELTA
            base_oid = data[pos:pos + 20].hex()
            pos += 20
            otype = "ref-delta"
        inflater = zlib.decompressobj()
        chunk = inflater.decompress(data[pos:])
        consumed = len(data[pos:]) - len(inflater.unused_data)
        pos += consumed
        if len(chunk) != size:
            raise ValueError("pack object size mismatch")
        if otype in ("ofs-delta", "ref-delta"):
            if otype == "ofs-delta":
                base_oid = offsets.get(entry_start - base_offset)
                otype = "ref-delta"
            pending.append((otype, base_oid, base_offset, chunk, pos))
        else:
            new_oid = STORE.put(otype, chunk)
            offsets[entry_start] = new_oid
            stored.append(new_oid)
    # resolve deltas (bases precede their deltas in a well-formed pack)
    progress = True
    while pending and progress:
        progress = False
        for item in list(pending):
            kind, base_oid, base_offset, delta, _ = item
            base_data = None
            base_type = None
            if kind == "ref-delta" and STORE.has(base_oid):
                base_type, base_data = STORE.get(base_oid)
            if base_data is not None:
                resolved = apply_delta(base_data, delta)
                stored.append(STORE.put(base_type, resolved))
                pending.remove(item)
                progress = True
    return stored


def pack_for_fetch(wants, haves, shallow_roots, depth, since=None):
    """Objects to send for a fetch, plus the new shallow boundary list."""
    excluded = set()
    for have in haves:
        if have in shallow_roots:
            # a shallow "have" covers only the boundary commit itself; the
            # client does not hold its history, so ancestors stay sendable
            continue
        if STORE.has(have) and STORE.get(have)[0] == "commit":
            for oid in ancestors_of(have):
                excluded.add(oid)
                tree_oid = commit_tree_oid(oid)
                if tree_oid:
                    excluded.update(tree_closure(tree_oid))
    shallow_excluded = set()
    for root in shallow_roots:
        if STORE.has(root) and STORE.get(root)[0] == "commit":
            shallow_excluded.add(root)
            tree_oid = commit_tree_oid(root)
            if tree_oid:
                shallow_excluded.update(tree_closure(tree_oid))
    included_commits = {}
    queue = []
    for want in wants:
        if STORE.has(want) and STORE.get(want)[0] == "commit" and want not in excluded:
            queue.append((want, 0))
    while queue:
        oid, dist = queue.pop(0)
        if oid in included_commits and included_commits[oid] <= dist:
            continue
        if oid in excluded or not STORE.has(oid):
            continue
        if depth is not None and dist >= depth and oid not in shallow_excluded:
            continue
        included_commits[oid] = dist
        if depth is None or dist < depth:
            for parent in parse_commit(STORE.get(oid)[1])["parents"]:
                if since is not None and STORE.has(parent) and commit_ts(parent) < since:
                    continue
                queue.append((parent, dist + 1))
    objects = []
    seen = set()

    def add(oid):
        if oid not in seen and oid not in excluded and oid not in shallow_excluded and STORE.has(oid):
            seen.add(oid)
            objects.append(oid)

    extra_wants = []
    for want in wants:
        if STORE.has(want) and STORE.get(want)[0] != "commit":
            extra_wants.append(want)
    for oid in extra_wants:
        otype = STORE.get(oid)[0]
        if otype == "tag":
            add(oid)
            target = STORE.get(oid)[1].split(b"\n")[0].split(b" ")[1].decode()
            if STORE.has(target) and STORE.get(target)[0] == "commit" and target not in included_commits and target not in excluded:
                included_commits[target] = 0
        elif otype in ("tree", "blob"):
            if otype == "tree":
                for sub in tree_closure(oid):
                    add(sub)
            else:
                add(oid)
    for oid in included_commits:
        add(oid)
    for oid in list(included_commits):
        tree_oid = commit_tree_oid(oid)
        if tree_oid:
            for sub in tree_closure(tree_oid):
                add(sub)
    # annotated tags pointing at included commits travel with a full clone
    if depth is None and not haves:
        for oid, (otype, data) in list(STORE.objects.items()):
            if otype == "tag":
                target_line = data.split(b"\n", 1)[0]
                if target_line.startswith(b"object "):
                    target = target_line[7:].decode()
                    if target in included_commits:
                        add(oid)
    boundary = []
    if depth is not None or shallow_roots or since is not None:
        for oid in included_commits:
            for parent in parse_commit(STORE.get(oid)[1])["parents"]:
                if parent not in included_commits and STORE.has(parent):
                    boundary.append(oid)
                    break
    return objects, boundary


def tree_closure(tree_oid):
    out = []
    seen = set()
    stack = [tree_oid]
    while stack:
        oid = stack.pop()
        if oid in seen or not STORE.has(oid):
            continue
        seen.add(oid)
        out.append(oid)
        found = STORE.get(oid)
        if found[0] == "tree":
            for mode, _, child in parse_tree(found[1]):
                stack.append(child)
    return out


# --------------------------------------------------------------------------
# forge state and REST serializers
# --------------------------------------------------------------------------


class Forge:
    def __init__(self):
        self.repos = {}
        self.next_repo_dbid = 5000
        self.next_object_id = 1
        self.request_seq = 0
        self.pack_cache = {}

    def alloc_id(self):
        value = self.next_object_id
        self.next_object_id += 1
        return value

    def create_repo(self, name, private, description):
        repo = {
            "name": name,
            "private": bool(private),
            "description": description or "",
            "dbid": self.next_repo_dbid,
            "created_ts": CLOCK.tick(),
            "refs": {},
            "labels": {},
            "issues": {},
            "pulls": {},
            "next_number": 1,
        }
        self.next_repo_dbid += 1
        self.repos[name] = repo
        return repo

    def get_repo(self, owner, name):
        if owner != VIEWER_LOGIN:
            return None
        return self.repos.get(name)


FORGE = Forge()


def global_id(type_name, key):
    return base64.b64encode(("%s:%s" % (type_name, key)).encode()).decode()


def parse_global_id(value):
    try:
        decoded = base64.b64decode(value.encode()).decode()
        type_name, _, key = decoded.partition(":")
        return type_name, key
    except Exception:
        return None, None


def user_json():
    return {
        "login": VIEWER_LOGIN,
        "id": VIEWER_ID,
        "node_id": global_id("User", VIEWER_ID),
        "type": "User",
        "site_admin": False,
        "name": VIEWER_NAME,
        "email": VIEWER_EMAIL,
    }


def repo_json(repo):
    ts = iso_of(repo["created_ts"])
    return {
        "id": repo["dbid"],
        "node_id": global_id("Repository", repo["dbid"]),
        "name": repo["name"],
        "full_name": "%s/%s" % (VIEWER_LOGIN, repo["name"]),
        "private": repo["private"],
        "owner": user_json(),
        "description": repo["description"],
        "fork": False,
        "default_branch": "main",
        "created_at": ts,
        "updated_at": ts,
        "pushed_at": ts,
        "size": 0,
        "stargazers_count": 0,
        "watchers_count": 0,
        "forks_count": 0,
        "open_issues_count": sum(1 for i in repo["issues"].values() if i["state"] == "open"),
        "has_issues": True,
        "has_pull_requests": True,
        "visibility": "private" if repo["private"] else "public",
    }


def label_json(label):
    return {
        "id": label["id"],
        "node_id": global_id("Label", label["id"]),
        "name": label["name"],
        "color": label["color"],
        "default": False,
        "description": None,
    }


def issue_json(repo, issue):
    return {
        "id": issue["id"],
        "node_id": global_id("Issue", "%d:%d" % (repo["dbid"], issue["number"])),
        "number": issue["number"],
        "title": issue["title"],
        "body": issue["body"],
        "state": issue["state"],
        "labels": [label_json(repo["labels"][name]) for name in issue["labels"] if name in repo["labels"]],
        "user": user_json(),
        "assignee": None,
        "assignees": [],
        "comments": len(issue["comments"]),
        "created_at": iso_of(issue["created_ts"]),
        "updated_at": iso_of(issue["updated_ts"]),
        "closed_at": iso_of(issue["closed_ts"]) if issue.get("closed_ts") else None,
    }


def comment_json(repo, issue, comment):
    return {
        "id": comment["id"],
        "node_id": global_id("IssueComment", "%d:%d:%d" % (repo["dbid"], issue["number"], comment["id"])),
        "body": comment["body"],
        "user": user_json(),
        "created_at": iso_of(comment["created_ts"]),
        "updated_at": iso_of(comment["created_ts"]),
    }


def branch_tip(repo, branch):
    return repo["refs"].get("refs/heads/" + branch)


def pull_json(repo, pull):
    head_sha = branch_tip(repo, pull["head"]) or pull["head_sha"]
    base_sha = branch_tip(repo, pull["base"]) or pull["base_sha"]
    state = "closed" if (pull["merged"] or pull["state"] == "closed") else "open"
    return {
        "id": pull["id"],
        "node_id": global_id("PullRequest", "%d:%d" % (repo["dbid"], pull["number"])),
        "number": pull["number"],
        "state": state,
        "title": pull["title"],
        "body": pull["body"],
        "user": user_json(),
        "merged": pull["merged"],
        "merged_at": iso_of(pull["merged_ts"]) if pull.get("merged_ts") else None,
        "merge_commit_sha": pull.get("merge_commit_sha"),
        "head": {
            "ref": pull["head"],
            "sha": head_sha,
            "label": "%s:%s" % (VIEWER_LOGIN, pull["head"]),
            "user": user_json(),
            "repo": repo_json(repo),
        },
        "base": {
            "ref": pull["base"],
            "sha": base_sha,
            "label": "%s:%s" % (VIEWER_LOGIN, pull["base"]),
            "user": user_json(),
            "repo": repo_json(repo),
        },
        "created_at": iso_of(pull["created_ts"]),
        "updated_at": iso_of(pull["updated_ts"]),
        "closed_at": iso_of(pull["closed_ts"]) if pull.get("closed_ts") else None,
        "draft": False,
    }


def commit_json(oid):
    parsed = parse_commit(STORE.get(oid)[1])

    def person(line):
        match = re.match(r"^(.*) <(.*)> (\d+) ([+-]\d{4})$", line or "")
        if not match:
            return {"name": "", "email": "", "date": iso_of(CLOCK.value)}
        return {"name": match.group(1), "email": match.group(2), "date": iso_of(int(match.group(3)))}

    return {
        "sha": oid,
        "node_id": global_id("Commit", oid),
        "commit": {
            "author": person(parsed["author"]),
            "committer": person(parsed["committer"]),
            "message": parsed["message"],
            "tree": {"sha": parsed["tree"]},
            "comment_count": 0,
        },
        "author": user_json(),
        "committer": user_json(),
        "parents": [{"sha": parent} for parent in parsed["parents"]],
    }


# --------------------------------------------------------------------------
# domain operations shared by REST, GraphQL and MCP
# --------------------------------------------------------------------------


def commit_files(repo, branch, changes, message):
    """Apply {path: bytes-or-None} as one commit on a branch; returns its oid."""
    ref = "refs/heads/" + branch
    tip = repo["refs"].get(ref)
    flat = commit_flat(tip) if tip else {}
    for path, data in changes.items():
        if data is None:
            flat.pop(path, None)
        else:
            flat[path] = ("100644", STORE.put("blob", data))
    tree_oid = tree_from_flat(flat)
    ts = CLOCK.tick()
    ident = viewer_ident(ts)
    parents = [tip] if tip else []
    commit_oid = STORE.put("commit", build_commit(tree_oid, parents, ident, ident, message))
    repo["refs"][ref] = commit_oid
    FORGE.pack_cache.clear()
    return commit_oid


def resolve_ref(repo, ref_value):
    """A branch name, a full ref, HEAD, or a raw commit oid -> commit oid."""
    if ref_value is None:
        ref_value = "main"
    if ref_value == "HEAD":
        ref_value = "main"
    if repo["refs"].get("refs/heads/" + ref_value):
        return repo["refs"]["refs/heads/" + ref_value]
    if repo["refs"].get(ref_value):
        return repo["refs"][ref_value]
    if re.fullmatch(r"[0-9a-f]{40}", ref_value or "") and STORE.has(ref_value):
        return ref_value
    return None


def pull_files(repo, pull):
    """Changed paths between the pull's merge base and its head tip."""
    head_sha = branch_tip(repo, pull["head"]) or pull["head_sha"]
    base_sha = branch_tip(repo, pull["base"]) or pull["base_sha"]
    base_commit = merge_base_of(base_sha, head_sha) or base_sha
    base_flat = commit_flat(base_commit)
    head_flat = commit_flat(head_sha)
    files = []
    for path in sorted(set(base_flat) | set(head_flat)):
        before = base_flat.get(path)
        after = head_flat.get(path)
        if before == after:
            continue
        before_lines = []
        after_lines = []
        if before is not None:
            before_lines = STORE.get(before[1])[1].decode("utf-8", "replace").splitlines()
        if after is not None:
            after_lines = STORE.get(after[1])[1].decode("utf-8", "replace").splitlines()
        additions = 0
        deletions = 0
        for tag, i1, i2, j1, j2 in difflib.SequenceMatcher(None, before_lines, after_lines).get_opcodes():
            if tag in ("insert", "replace"):
                additions += j2 - j1
            if tag in ("delete", "replace"):
                deletions += i2 - i1
        status = "added" if before is None else ("removed" if after is None else "modified")
        files.append({
            "filename": path,
            "status": status,
            "additions": additions,
            "deletions": deletions,
            "changes": additions + deletions,
            "sha": after[1] if after else before[1],
        })
    return files


def merge_conflicts(repo, pull):
    head_sha = branch_tip(repo, pull["head"]) or pull["head_sha"]
    base_sha = branch_tip(repo, pull["base"]) or pull["base_sha"]
    if head_sha is None or base_sha is None:
        return ["<missing-ref>"]
    base_commit = merge_base_of(base_sha, head_sha)
    if base_commit is None:
        return ["<no-merge-base>"]
    fb = commit_flat(base_commit)
    fh = commit_flat(head_sha)
    ft = commit_flat(base_sha)
    conflicts = []
    for path in set(fb) | set(fh):
        base_v = fb.get(path)
        head_v = fh.get(path)
        target_v = ft.get(path)
        if head_v == base_v or target_v == base_v or head_v == target_v:
            continue
        conflicts.append(path)
    return conflicts


def merge_pull(repo, pull, method):
    """Merge a pull request; returns the new base tip oid, or None on conflict."""
    if merge_conflicts(repo, pull):
        return None
    head_sha = branch_tip(repo, pull["head"]) or pull["head_sha"]
    base_sha = branch_tip(repo, pull["base"]) or pull["base_sha"]
    base_commit = merge_base_of(base_sha, head_sha) or base_sha
    fb = commit_flat(base_commit)
    fh = commit_flat(head_sha)
    merged = dict(commit_flat(base_sha))
    for path in set(fb) | set(fh):
        if fh.get(path) == fb.get(path):
            continue
        if fh.get(path) is None:
            merged.pop(path, None)
        else:
            merged[path] = fh[path]
    tree_oid = tree_from_flat(merged)
    ts = CLOCK.tick()
    ident = viewer_ident(ts)
    if (method or "squash").lower() == "merge":
        parents = [base_sha, head_sha]
        message = "Merge pull request #%d from %s" % (pull["number"], pull["head"])
    else:
        parents = [base_sha]
        message = pull["title"]
    commit_oid = STORE.put("commit", build_commit(tree_oid, parents, ident, ident, message))
    repo["refs"]["refs/heads/" + pull["base"]] = commit_oid
    pull["merged"] = True
    pull["state"] = "closed"
    pull["merged_ts"] = ts
    pull["closed_ts"] = ts
    pull["updated_ts"] = ts
    pull["merge_commit_sha"] = commit_oid
    FORGE.pack_cache.clear()
    return commit_oid


def create_issue(repo, title, body, label_names):
    number = repo["next_number"]
    repo["next_number"] += 1
    ts = CLOCK.tick()
    issue = {
        "id": FORGE.alloc_id(),
        "number": number,
        "title": title or "",
        "body": body or "",
        "state": "open",
        "labels": list(label_names or []),
        "comments": [],
        "created_ts": ts,
        "updated_ts": ts,
        "closed_ts": None,
    }
    repo["issues"][number] = issue
    return issue


def create_pull(repo, title, body, head, base):
    number = repo["next_number"]
    repo["next_number"] += 1
    ts = CLOCK.tick()
    pull = {
        "id": FORGE.alloc_id(),
        "number": number,
        "title": title or "",
        "body": body or "",
        "state": "open",
        "merged": False,
        "head": head,
        "base": base,
        "head_sha": branch_tip(repo, head),
        "base_sha": branch_tip(repo, base),
        "merge_commit_sha": None,
        "merged_ts": None,
        "closed_ts": None,
        "created_ts": ts,
        "updated_ts": ts,
    }
    repo["pulls"][number] = pull
    return pull


# --------------------------------------------------------------------------
# HTTP reply plumbing
# --------------------------------------------------------------------------


class Reply:
    def __init__(self, status, body=b"", content_type="application/json; charset=utf-8", headers=None):
        self.status = status
        self.body = body
        self.content_type = content_type
        self.headers = headers or {}


def json_reply(status, obj, headers=None):
    return Reply(status, json.dumps(obj).encode(), "application/json; charset=utf-8", headers)


def not_found(message="Not Found"):
    return json_reply(404, {"message": message})


def validation_failed(message="Validation Failed"):
    return json_reply(422, {"message": message})


def parse_json_body(body_bytes):
    if not body_bytes:
        return {}, None
    try:
        value = json.loads(body_bytes.decode("utf-8"))
    except Exception:
        return None, json_reply(400, {"message": "Problems parsing JSON"})
    if not isinstance(value, dict):
        return {}, None
    return value, None


def paginate(items, query, path, host):
    try:
        per_page = int(query.get("per_page", ["30"])[0])
    except ValueError:
        per_page = 30
    try:
        page = int(query.get("page", ["1"])[0])
    except ValueError:
        page = 1
    per_page = max(1, min(per_page, 100))
    page = max(1, page)
    total_pages = max(1, (len(items) + per_page - 1) // per_page)
    start = (page - 1) * per_page
    sliced = items[start:start + per_page]
    links = []

    def link_url(page_no):
        return "http://%s%s?per_page=%d&page=%d" % (host, path, per_page, page_no)

    if page < total_pages:
        links.append('<%s>; rel="next"' % link_url(page + 1))
        links.append('<%s>; rel="last"' % link_url(total_pages))
    if page > 1:
        links.append('<%s>; rel="prev"' % link_url(page - 1))
        links.append('<%s>; rel="first"' % link_url(1))
    headers = {"Link": ", ".join(links)} if links else {}
    return sliced, headers


# --------------------------------------------------------------------------
# REST v3
# --------------------------------------------------------------------------


def rest_reply(method, segments, query, body, host, raw_path):
    if not segments:
        if method == "GET":
            root = {
                "current_user_url": "/api/v3/user",
                "rate_limit_url": "/api/v3/rate_limit",
                "repository_url": "/api/v3/repos/{owner}/{repo}",
                "issue_search_url": "/api/v3/search/issues",
            }
            return json_reply(200, root, {"X-Oauth-Scopes": "repo, read:org"})
        return not_found()
    head = segments[0]
    if head == "meta" and method == "GET":
        return json_reply(200, {"installed_version": FORGE_VERSION})
    if head == "user":
        if len(segments) == 1 and method == "GET":
            return json_reply(200, user_json())
        if len(segments) == 2 and segments[1] == "repos":
            if method == "GET":
                items = [repo_json(r) for r in FORGE.repos.values()]
                sliced, headers = paginate(items, query, raw_path, host)
                return json_reply(200, sliced, headers)
            if method == "POST":
                data, err = parse_json_body(body)
                if err:
                    return err
                name = data.get("name")
                if not name:
                    return validation_failed("Repository creation failed.")
                if name in FORGE.repos:
                    return json_reply(422, {"message": "Repository creation failed.", "errors": [{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}]})
                repo = FORGE.create_repo(name, data.get("private", False), data.get("description", ""))
                return json_reply(201, repo_json(repo))
        return not_found()
    if head == "users" and len(segments) == 2 and method == "GET":
        if segments[1] == VIEWER_LOGIN:
            return json_reply(200, user_json())
        return not_found()
    if head != "repos" or len(segments) < 3:
        return not_found()
    owner, name = segments[1], segments[2]
    repo = FORGE.get_repo(owner, name)
    if repo is None:
        return not_found()
    rest = segments[3:]
    if not rest:
        if method == "GET":
            return json_reply(200, repo_json(repo))
        if method == "PATCH":
            data, err = parse_json_body(body)
            if err:
                return err
            if "description" in data:
                repo["description"] = data["description"] or ""
            if "private" in data:
                repo["private"] = bool(data["private"])
            if "name" in data and data["name"] != repo["name"]:
                FORGE.repos.pop(repo["name"], None)
                repo["name"] = data["name"]
                FORGE.repos[repo["name"]] = repo
            return json_reply(200, repo_json(repo))
        if method == "DELETE":
            FORGE.repos.pop(repo["name"], None)
            return Reply(204, b"", "application/json; charset=utf-8")
        return not_found()
    return rest_repo_reply(method, repo, rest, query, body, host, raw_path)


def rest_repo_reply(method, repo, rest, query, body, host, raw_path):
    area = rest[0]
    if area == "branches":
        if len(rest) == 1 and method == "GET":
            items = []
            for ref, oid in sorted(repo["refs"].items()):
                if ref.startswith("refs/heads/"):
                    items.append({"name": ref[len("refs/heads/"):], "commit": {"sha": oid}, "protected": False})
            sliced, headers = paginate(items, query, raw_path, host)
            return json_reply(200, sliced, headers)
        if len(rest) >= 2 and method == "GET":
            branch = "/".join(rest[1:])
            tip = branch_tip(repo, branch)
            if tip is None:
                return not_found()
            return json_reply(200, {"name": branch, "commit": commit_json(tip), "protected": False})
        return not_found()
    if area == "commits":
        if len(rest) == 1 and method == "GET":
            ref_value = query.get("sha", [None])[0]
            tip = resolve_ref(repo, ref_value)
            if tip is None:
                return json_reply(200, [])
            items = [commit_json(oid) for oid in ancestors_of(tip)]
            sliced, headers = paginate(items, query, raw_path, host)
            return json_reply(200, sliced, headers)
        if len(rest) == 2 and method == "GET":
            if STORE.has(rest[1]) and STORE.get(rest[1])[0] == "commit":
                return json_reply(200, commit_json(rest[1]))
            return not_found()
        return not_found()
    if area == "contents":
        return contents_reply(method, repo, rest[1:], query, body)
    if area == "git":
        return git_api_reply(method, repo, rest[1:], body)
    if area == "labels":
        return labels_reply(method, repo, rest[1:], body)
    if area == "issues":
        return issues_reply(method, repo, rest[1:], query, body, host, raw_path)
    if area == "pulls":
        return pulls_reply(method, repo, rest[1:], query, body, host, raw_path)
    return not_found()


def contents_reply(method, repo, path_parts, query, body):
    path = "/".join(path_parts)
    if method == "GET":
        ref_value = query.get("ref", [None])[0]
        tip = resolve_ref(repo, ref_value)
        if tip is None:
            if path == "":
                return json_reply(200, [])
            return not_found()
        flat = commit_flat(tip)
        if path and path in flat:
            mode, blob_oid = flat[path]
            data = STORE.get(blob_oid)[1]
            encoded = base64.encodebytes(data).decode()
            return json_reply(200, {
                "type": "file",
                "encoding": "base64",
                "content": encoded,
                "sha": blob_oid,
                "size": len(data),
                "name": path.split("/")[-1],
                "path": path,
            })
        prefix = (path + "/") if path else ""
        entries = {}
        for file_path, (mode, blob_oid) in flat.items():
            if not file_path.startswith(prefix):
                continue
            remainder = file_path[len(prefix):]
            first = remainder.split("/")[0]
            if "/" in remainder:
                if first not in entries:
                    sub_oid = find_subtree_oid(repo, tip, prefix + first)
                    entries[first] = {"type": "dir", "name": first, "path": prefix + first, "sha": sub_oid, "size": 0}
            else:
                entries[first] = {"type": "file", "name": first, "path": file_path, "sha": blob_oid, "size": len(STORE.get(blob_oid)[1])}
        if path and not entries:
            return not_found()
        return json_reply(200, [entries[key] for key in sorted(entries)])
    data, err = parse_json_body(body)
    if err:
        return err
    branch = data.get("branch") or "main"
    ref = "refs/heads/" + branch
    if method == "PUT":
        if ref not in repo["refs"] and repo["refs"]:
            return not_found("Branch not found")
        if not path:
            return validation_failed("path is required")
        content_b64 = data.get("content", "")
        try:
            raw = base64.b64decode(content_b64)
        except Exception:
            return json_reply(400, {"message": "content is not valid base64"})
        tip = repo["refs"].get(ref)
        flat = commit_flat(tip) if tip else {}
        existing = flat.get(path)
        provided_sha = data.get("sha")
        if existing is not None:
            if provided_sha is not None and provided_sha != existing[1]:
                return json_reply(409, {"message": "SHA does not match"})
            if provided_sha is None:
                return validation_failed("sha is required when updating a file")
            status = 200
        else:
            status = 201
        blob_oid = STORE.put("blob", raw)
        commit_oid = commit_files(repo, branch, {path: raw}, data.get("message", "Update %s" % path))
        return json_reply(status, {
            "content": {"name": path.split("/")[-1], "path": path, "sha": blob_oid, "size": len(raw), "encoding": "base64", "type": "file"},
            "commit": commit_json(commit_oid),
        })
    if method == "DELETE":
        tip = repo["refs"].get(ref)
        if tip is None:
            return not_found()
        flat = commit_flat(tip)
        existing = flat.get(path)
        if existing is None:
            return not_found()
        provided_sha = data.get("sha")
        if provided_sha is not None and provided_sha != existing[1]:
            return json_reply(409, {"message": "SHA does not match"})
        commit_oid = commit_files(repo, branch, {path: None}, data.get("message", "Delete %s" % path))
        return json_reply(200, {"content": None, "commit": commit_json(commit_oid)})
    return not_found()


def find_subtree_oid(repo, tip, dir_path):
    tree_oid = commit_tree_oid(tip)
    for part in dir_path.strip("/").split("/"):
        if tree_oid is None:
            return ZERO_OID
        found = None
        for mode, entry_name, oid in parse_tree(STORE.get(tree_oid)[1]):
            if entry_name == part and mode == "40000":
                found = oid
        tree_oid = found
    return tree_oid or ZERO_OID


def git_api_reply(method, repo, rest, body):
    if rest and rest[0] == "refs":
        if len(rest) == 1:
            if method == "POST":
                data, err = parse_json_body(body)
                if err:
                    return err
                ref = data.get("ref", "")
                sha = data.get("sha", "")
                if not ref.startswith("refs/") or not STORE.has(sha):
                    return validation_failed("Invalid ref or sha")
                if ref in repo["refs"]:
                    return validation_failed("Reference already exists")
                repo["refs"][ref] = sha
                FORGE.pack_cache.clear()
                return json_reply(201, {"ref": ref, "node_id": global_id("Ref", ref), "object": {"sha": sha, "type": "commit"}})
            if method == "GET":
                items = [{"ref": ref, "object": {"sha": oid, "type": "commit"}} for ref, oid in sorted(repo["refs"].items())]
                return json_reply(200, items)
        if len(rest) >= 2 and method == "GET":
            ref = "refs/" + "/".join(rest[1:])
            oid = repo["refs"].get(ref)
            if oid is None:
                return not_found()
            return json_reply(200, {"ref": ref, "object": {"sha": oid, "type": "commit"}})
    return not_found()


def labels_reply(method, repo, rest, body):
    if not rest:
        if method == "POST":
            data, err = parse_json_body(body)
            if err:
                return err
            name = data.get("name")
            if not name:
                return validation_failed("name is required")
            if name in repo["labels"]:
                return validation_failed("Label already exists")
            label = {"id": FORGE.alloc_id(), "name": name, "color": (data.get("color") or "ededed").lstrip("#")}
            repo["labels"][name] = label
            return json_reply(201, label_json(label))
        if method == "GET":
            return json_reply(200, [label_json(l) for l in repo["labels"].values()])
    if len(rest) == 1 and method == "GET":
        label = repo["labels"].get(rest[0])
        if label is None:
            return not_found()
        return json_reply(200, label_json(label))
    return not_found()


def issues_reply(method, repo, rest, query, body, host, raw_path):
    if not rest:
        if method == "POST":
            data, err = parse_json_body(body)
            if err:
                return err
            label_names = []
            for entry in data.get("labels") or []:
                label_names.append(entry["name"] if isinstance(entry, dict) else entry)
            for label_name in label_names:
                if label_name not in repo["labels"]:
                    return validation_failed("Validation Failed: label '%s' does not exist" % label_name)
            issue = create_issue(repo, data.get("title"), data.get("body"), label_names)
            return json_reply(201, issue_json(repo, issue))
        if method == "GET":
            state = query.get("state", ["open"])[0]
            items = [i for i in repo["issues"].values() if state == "all" or i["state"] == state]
            direction = query.get("direction", ["desc"])[0]
            items.sort(key=lambda i: i["number"], reverse=(direction != "asc"))
            rendered = [issue_json(repo, i) for i in items]
            sliced, headers = paginate(rendered, query, raw_path, host)
            return json_reply(200, sliced, headers)
        return not_found()
    try:
        number = int(rest[0])
    except ValueError:
        return not_found()
    issue = repo["issues"].get(number)
    if len(rest) == 1:
        if issue is None:
            return not_found()
        if method == "GET":
            return json_reply(200, issue_json(repo, issue))
        if method == "PATCH":
            data, err = parse_json_body(body)
            if err:
                return err
            if "title" in data:
                issue["title"] = data["title"] or ""
            if "body" in data:
                issue["body"] = data["body"] or ""
            if "labels" in data:
                names = [e["name"] if isinstance(e, dict) else e for e in data["labels"] or []]
                for label_name in names:
                    if label_name not in repo["labels"]:
                        return validation_failed("Validation Failed: unknown label")
                issue["labels"] = names
            if "state" in data:
                issue["state"] = data["state"]
                if data["state"] == "closed":
                    issue["closed_ts"] = CLOCK.tick()
                else:
                    issue["closed_ts"] = None
            issue["updated_ts"] = CLOCK.tick()
            return json_reply(200, issue_json(repo, issue))
        return not_found()
    if len(rest) == 2 and rest[1] == "comments":
        if issue is None:
            return not_found()
        if method == "POST":
            data, err = parse_json_body(body)
            if err:
                return err
            comment = {"id": FORGE.alloc_id(), "body": data.get("body") or "", "created_ts": CLOCK.tick()}
            issue["comments"].append(comment)
            return json_reply(201, comment_json(repo, issue, comment))
        if method == "GET":
            items = [comment_json(repo, issue, c) for c in issue["comments"]]
            sliced, headers = paginate(items, query, raw_path, host)
            return json_reply(200, sliced, headers)
    return not_found()


def pulls_reply(method, repo, rest, query, body, host, raw_path):
    if not rest:
        if method == "POST":
            data, err = parse_json_body(body)
            if err:
                return err
            head = data.get("head", "")
            base = data.get("base", "main")
            head = head.split(":")[-1]
            base = base.split(":")[-1]
            if branch_tip(repo, head) is None or branch_tip(repo, base) is None:
                return validation_failed("Validation Failed: head or base branch does not exist")
            pull = create_pull(repo, data.get("title"), data.get("body"), head, base)
            return json_reply(201, pull_json(repo, pull))
        if method == "GET":
            state = query.get("state", ["open"])[0]
            items = []
            for pull in repo["pulls"].values():
                rendered_state = "closed" if (pull["merged"] or pull["state"] == "closed") else "open"
                if state == "all" or rendered_state == state:
                    items.append(pull)
            items.sort(key=lambda p: p["number"])
            rendered = [pull_json(repo, p) for p in items]
            sliced, headers = paginate(rendered, query, raw_path, host)
            return json_reply(200, sliced, headers)
        return not_found()
    try:
        number = int(rest[0])
    except ValueError:
        return not_found()
    pull = repo["pulls"].get(number)
    if len(rest) == 1:
        if pull is None:
            return not_found()
        if method == "GET":
            return json_reply(200, pull_json(repo, pull))
        if method == "PATCH":
            data, err = parse_json_body(body)
            if err:
                return err
            if "title" in data:
                pull["title"] = data["title"] or ""
            if "body" in data:
                pull["body"] = data["body"] or ""
            if "state" in data:
                pull["state"] = data["state"]
                if data["state"] == "closed":
                    pull["closed_ts"] = CLOCK.tick()
            pull["updated_ts"] = CLOCK.tick()
            return json_reply(200, pull_json(repo, pull))
        return not_found()
    if pull is None:
        return not_found()
    if len(rest) == 2 and rest[1] == "files" and method == "GET":
        items = pull_files(repo, pull)
        sliced, headers = paginate(items, query, raw_path, host)
        return json_reply(200, sliced, headers)
    if len(rest) == 2 and rest[1] == "merge" and method == "PUT":
        data, err = parse_json_body(body)
        if err:
            return err
        if pull["merged"] or pull["state"] == "closed":
            return json_reply(405, {"message": "Pull Request is not mergeable"})
        merged_sha = merge_pull(repo, pull, data.get("merge_method") or "merge")
        if merged_sha is None:
            return json_reply(405, {"message": "Pull Request is not mergeable"})
        return json_reply(200, {"sha": merged_sha, "merged": True, "message": "Pull Request successfully merged"})
    return not_found()


# --------------------------------------------------------------------------
# GraphQL v4
# --------------------------------------------------------------------------

GQL_TOKEN_RE = re.compile(r"""
    (?P<skip>[\s,]+|\#[^\n]*)
  | (?P<spread>\.\.\.)
  | (?P<name>[_A-Za-z][_0-9A-Za-z]*)
  | (?P<float>-?\d+\.\d+(?:[eE][+-]?\d+)?)
  | (?P<int>-?\d+)
  | (?P<string>"(?:\\.|[^"\\])*")
  | (?P<punct>[!$&():=@\[\]{}|=])
""", re.VERBOSE)

GQL_TYPE_FIELDS = {
    "Query": ["viewer", "repository", "node", "__type", "__schema"],
    "Mutation": ["createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"],
    "User": ["id", "login", "name", "email", "url"],
    "Repository": ["id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef",
                   "viewerPermission", "hasIssuesEnabled", "hasPullRequestsEnabled", "isPrivate",
                   "issue", "issues", "pullRequest", "pullRequests", "url", "createdAt", "updatedAt"],
    "Ref": ["name", "target"],
    "Issue": ["id", "number", "title", "state", "body", "author", "labels", "comments",
              "createdAt", "updatedAt", "url"],
    "IssueConnection": ["nodes", "pageInfo", "totalCount"],
    "PullRequest": ["id", "number", "title", "body", "state", "merged", "mergeable",
                    "headRefName", "baseRefName", "files", "author", "createdAt", "updatedAt"],
    "PullRequestConnection": ["nodes", "pageInfo", "totalCount"],
    "PullRequestChangedFile": ["path"],
    "PullRequestChangedFileConnection": ["nodes", "pageInfo", "totalCount"],
    "PageInfo": ["hasNextPage", "endCursor", "startCursor", "hasPreviousPage"],
    "IssueComment": ["id", "body", "author", "createdAt"],
    "CreateIssuePayload": ["issue", "clientMutationId"],
    "CloseIssuePayload": ["issue", "clientMutationId"],
    "CreatePullRequestPayload": ["pullRequest", "clientMutationId"],
    "MergePullRequestPayload": ["pullRequest", "clientMutationId"],
    "AddCommentPayload": ["commentEdge", "clientMutationId"],
    "IssueCommentEdge": ["node", "cursor"],
    "__Type": ["name", "kind", "fields", "enumValues", "description"],
    "__Field": ["name", "description", "isDeprecated", "deprecationReason"],
    "__EnumValue": ["name", "description", "isDeprecated", "deprecationReason"],
    "__Schema": ["types", "queryType", "mutationType"],
}

GQL_ENUM_VALUES = {
    "IssueState": ["OPEN", "CLOSED"],
    "PullRequestState": ["OPEN", "CLOSED", "MERGED"],
    "MergeableState": ["MERGEABLE", "CONFLICTING", "UNKNOWN"],
    "PullRequestMergeMethod": ["MERGE", "SQUASH", "REBASE"],
    "RepositoryPermission": ["ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"],
    "SearchType": ["ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE"],
    "IssueOrderField": ["CREATED_AT", "UPDATED_AT", "COMMENTS"],
    "OrderDirection": ["ASC", "DESC"],
}


class GqlError(Exception):
    pass


def gql_tokenize(text):
    tokens = []
    pos = 0
    while pos < len(text):
        match = GQL_TOKEN_RE.match(text, pos)
        if not match:
            raise GqlError("unexpected character %r" % text[pos])
        pos = match.end()
        kind = match.lastgroup
        if kind == "skip":
            continue
        tokens.append((kind, match.group()))
    tokens.append(("eof", ""))
    return tokens


class GqlParser:
    def __init__(self, text):
        self.tokens = gql_tokenize(text)
        self.pos = 0

    def peek(self):
        return self.tokens[self.pos]

    def next(self):
        token = self.tokens[self.pos]
        self.pos += 1
        return token

    def expect_punct(self, value):
        kind, token = self.next()
        if kind != "punct" or token != value:
            raise GqlError("expected %r, got %r" % (value, token))

    def skip_punct(self, value):
        kind, token = self.peek()
        if kind == "punct" and token == value:
            self.pos += 1
            return True
        return False

    def parse_document(self):
        operations = []
        fragments = {}
        while self.peek()[0] != "eof":
            kind, token = self.peek()
            if kind == "punct" and token == "{":
                operations.append({"type": "query", "name": None, "var_defs": {}, "selections": self.parse_selection_set()})
            elif kind == "name" and token in ("query", "mutation", "subscription"):
                self.next()
                op = {"type": token, "name": None, "var_defs": {}, "selections": []}
                if self.peek()[0] == "name":
                    op["name"] = self.next()[1]
                if self.peek() == ("punct", "("):
                    op["var_defs"] = self.parse_variable_defs()
                self.skip_directives()
                op["selections"] = self.parse_selection_set()
                operations.append(op)
            elif kind == "name" and token == "fragment":
                self.next()
                frag_name = self.next()[1]
                self.next()  # 'on'
                type_condition = self.next()[1]
                self.skip_directives()
                fragments[frag_name] = {"type": type_condition, "selections": self.parse_selection_set()}
            else:
                raise GqlError("unexpected token %r" % token)
        return operations, fragments

    def skip_directives(self):
        while self.peek() == ("punct", "@"):
            self.next()
            self.next()
            if self.peek() == ("punct", "("):
                self.parse_arguments()

    def parse_variable_defs(self):
        defs = {}
        self.expect_punct("(")
        while self.peek() != ("punct", ")"):
            self.expect_punct("$")
            name = self.next()[1]
            self.expect_punct(":")
            self.parse_type()
            default = None
            if self.skip_punct("="):
                default = self.parse_value()
            defs[name] = default
            self.skip_directives()
        self.expect_punct(")")
        return defs

    def parse_type(self):
        if self.skip_punct("["):
            self.parse_type()
            self.expect_punct("]")
        else:
            self.next()
        self.skip_punct("!")

    def parse_selection_set(self):
        selections = []
        self.expect_punct("{")
        while self.peek() != ("punct", "}"):
            selections.append(self.parse_selection())
        self.expect_punct("}")
        return selections

    def parse_selection(self):
        kind, token = self.peek()
        if kind == "spread":
            self.next()
            if self.peek()[0] == "name" and self.peek()[1] == "on":
                self.next()
                type_condition = self.next()[1]
                self.skip_directives()
                return {"kind": "inline", "type": type_condition, "selections": self.parse_selection_set()}
            frag_name = self.next()[1]
            self.skip_directives()
            return {"kind": "spread", "name": frag_name}
        name = self.next()[1]
        alias = None
        if self.skip_punct(":"):
            alias = name
            name = self.next()[1]
        args = {}
        if self.peek() == ("punct", "("):
            args = self.parse_arguments()
        self.skip_directives()
        selections = []
        if self.peek() == ("punct", "{"):
            selections = self.parse_selection_set()
        return {"kind": "field", "alias": alias, "name": name, "args": args, "selections": selections}

    def parse_arguments(self):
        args = {}
        self.expect_punct("(")
        while self.peek() != ("punct", ")"):
            arg_name = self.next()[1]
            self.expect_punct(":")
            args[arg_name] = self.parse_value()
        self.expect_punct(")")
        return args

    def parse_value(self):
        kind, token = self.next()
        if kind == "punct" and token == "$":
            return ("var", self.next()[1])
        if kind == "punct" and token == "[":
            items = []
            while self.peek() != ("punct", "]"):
                items.append(self.parse_value())
            self.expect_punct("]")
            return ("list", items)
        if kind == "punct" and token == "{":
            obj = {}
            while self.peek() != ("punct", "}"):
                key = self.next()[1]
                self.expect_punct(":")
                obj[key] = self.parse_value()
            self.expect_punct("}")
            return ("object", obj)
        if kind == "string":
            return json.loads(token)
        if kind == "int":
            return int(token)
        if kind == "float":
            return float(token)
        if kind == "name":
            if token == "true":
                return True
            if token == "false":
                return False
            if token == "null":
                return None
            return ("enum", token)
        raise GqlError("unexpected value token %r" % token)


def gql_resolve_value(value, variables):
    if isinstance(value, tuple):
        kind = value[0]
        if kind == "var":
            return variables.get(value[1])
        if kind == "enum":
            return value[1]
        if kind == "list":
            return [gql_resolve_value(item, variables) for item in value[1]]
        if kind == "object":
            return {key: gql_resolve_value(item, variables) for key, item in value[1].items()}
    return value


def gql_validate(selections, type_name, fragments, errors):
    fields = GQL_TYPE_FIELDS.get(type_name)
    if fields is None:
        return
    for selection in selections:
        if selection["kind"] == "spread":
            fragment = fragments.get(selection["name"])
            if fragment:
                gql_validate(fragment["selections"], fragment["type"], fragments, errors)
            continue
        if selection["kind"] == "inline":
            gql_validate(selection["selections"], selection["type"], fragments, errors)
            continue
        name = selection["name"]
        if name == "__typename":
            continue
        if name not in fields:
            errors.append("Field '%s' doesn't exist on type '%s'" % (name, type_name))
            continue
        child_type = gql_child_type(type_name, name)
        if child_type and selection["selections"]:
            gql_validate(selection["selections"], child_type, fragments, errors)


def gql_child_type(type_name, field):
    mapping = {
        ("Query", "viewer"): "User",
        ("Query", "repository"): "Repository",
        ("Query", "node"): "Repository",
        ("Query", "__type"): "__Type",
        ("Repository", "owner"): "User",
        ("Repository", "defaultBranchRef"): "Ref",
        ("Repository", "issue"): "Issue",
        ("Repository", "issues"): "IssueConnection",
        ("Repository", "pullRequest"): "PullRequest",
        ("Repository", "pullRequests"): "PullRequestConnection",
        ("IssueConnection", "nodes"): "Issue",
        ("IssueConnection", "pageInfo"): "PageInfo",
        ("PullRequestConnection", "nodes"): "PullRequest",
        ("PullRequestConnection", "pageInfo"): "PageInfo",
        ("PullRequest", "files"): "PullRequestChangedFileConnection",
        ("PullRequest", "author"): "User",
        ("Issue", "author"): "User",
        ("PullRequestChangedFileConnection", "nodes"): "PullRequestChangedFile",
        ("PullRequestChangedFileConnection", "pageInfo"): "PageInfo",
        ("CreateIssuePayload", "issue"): "Issue",
        ("CloseIssuePayload", "issue"): "Issue",
        ("CreatePullRequestPayload", "pullRequest"): "PullRequest",
        ("MergePullRequestPayload", "pullRequest"): "PullRequest",
        ("AddCommentPayload", "commentEdge"): "IssueCommentEdge",
        ("IssueCommentEdge", "node"): "IssueComment",
        ("IssueComment", "author"): "User",
        ("__Type", "fields"): "__Field",
        ("__Type", "enumValues"): "__EnumValue",
    }
    return mapping.get((type_name, field))


def gql_tag(type_name, payload):
    return ("__gql__", type_name, payload)


def gql_is_tag(value):
    return isinstance(value, tuple) and len(value) == 3 and value[0] == "__gql__"


def gql_connection(tagged_items, args):
    first = args.get("first")
    after = args.get("after")
    start = 0
    if after:
        try:
            decoded = base64.b64decode(after.encode()).decode()
            start = int(decoded.split(":")[1]) + 1
        except Exception:
            start = 0
    sliced = tagged_items[start:]
    if first is not None:
        sliced = sliced[:int(first)]
    end_index = start + len(sliced) - 1
    page = {
        "hasNextPage": (start + len(sliced)) < len(tagged_items),
        "endCursor": base64.b64encode(("cursor:%d" % end_index).encode()).decode() if sliced else None,
        "startCursor": base64.b64encode(("cursor:%d" % start).encode()).decode() if sliced else None,
        "hasPreviousPage": start > 0,
    }
    return {"items": sliced, "total": len(tagged_items), "page": page}


def gql_repo_by_dbid(dbid):
    for repo in FORGE.repos.values():
        if repo["dbid"] == dbid:
            return repo
    return None


def gql_issues_connection(repo, args):
    items = list(repo["issues"].values())
    states = args.get("states")
    if states:
        wanted = {str(s).lower() for s in states}
        items = [i for i in items if i["state"] in wanted]
    order = args.get("orderBy") or {}
    direction = order.get("direction") or "DESC"
    field = order.get("field") or "CREATED_AT"
    if field == "COMMENTS":
        key = lambda i: len(i["comments"])
    elif field == "UPDATED_AT":
        key = lambda i: i["updated_ts"]
    else:
        key = lambda i: i["number"]
    items.sort(key=key, reverse=(direction == "DESC"))
    tagged = [gql_tag("Issue", (repo, i)) for i in items]
    conn = gql_connection(tagged, args)
    return gql_tag("IssueConnection", conn)


def gql_pulls_connection(repo, args):
    items = sorted(repo["pulls"].values(), key=lambda p: p["number"], reverse=True)
    states = args.get("states")
    if states:
        def state_of(p):
            return "MERGED" if p["merged"] else ("CLOSED" if p["state"] == "closed" else "OPEN")
        wanted = {str(s) for s in states}
        items = [p for p in items if state_of(p) in wanted]
    tagged = [gql_tag("PullRequest", (repo, p)) for p in items]
    conn = gql_connection(tagged, args)
    return gql_tag("PullRequestConnection", conn)


def gql_resolve_field(type_name, obj, field, args):
    if type_name == "Query":
        if field == "viewer":
            return gql_tag("User", None)
        if field == "repository":
            repo = FORGE.get_repo(args.get("owner"), args.get("name"))
            return gql_tag("Repository", repo) if repo else None
        if field == "node":
            decoded_type, key = parse_global_id(args.get("id") or "")
            if decoded_type == "Repository":
                repo = gql_repo_by_dbid(int(key)) if key and key.isdigit() else None
                return gql_tag("Repository", repo) if repo else None
            if decoded_type in ("Issue", "PullRequest"):
                parts = key.split(":")
                repo = gql_repo_by_dbid(int(parts[0])) if parts and parts[0].isdigit() else None
                if repo is None or len(parts) < 2:
                    return None
                number = int(parts[1])
                if decoded_type == "Issue" and number in repo["issues"]:
                    return gql_tag("Issue", (repo, repo["issues"][number]))
                if decoded_type == "PullRequest" and number in repo["pulls"]:
                    return gql_tag("PullRequest", (repo, repo["pulls"][number]))
            return None
        if field == "__type":
            name = args.get("name")
            if name in GQL_TYPE_FIELDS or name in GQL_ENUM_VALUES:
                return gql_tag("__Type", name)
            return None
        if field == "__schema":
            return gql_tag("__Schema", None)
    if type_name == "Mutation":
        input_data = args.get("input") or {}
        if field == "createIssue":
            _, key = parse_global_id(input_data.get("repositoryId") or "")
            repo = gql_repo_by_dbid(int(key)) if key and key.isdigit() else None
            if repo is None:
                raise GqlError("repository not found")
            issue = create_issue(repo, input_data.get("title"), input_data.get("body"), [])
            return gql_tag("CreateIssuePayload", (repo, issue))
        if field == "closeIssue":
            _, key = parse_global_id(input_data.get("issueId") or "")
            parts = key.split(":")
            repo = gql_repo_by_dbid(int(parts[0])) if parts and parts[0].isdigit() else None
            issue = repo["issues"].get(int(parts[1])) if repo and len(parts) > 1 else None
            if issue is None:
                raise GqlError("issue not found")
            issue["state"] = "closed"
            issue["closed_ts"] = CLOCK.tick()
            return gql_tag("CloseIssuePayload", (repo, issue))
        if field == "createPullRequest":
            _, key = parse_global_id(input_data.get("repositoryId") or "")
            repo = gql_repo_by_dbid(int(key)) if key and key.isdigit() else None
            if repo is None:
                raise GqlError("repository not found")
            pull = create_pull(repo, input_data.get("title"), input_data.get("body"),
                                input_data.get("headRefName"), input_data.get("baseRefName"))
            return gql_tag("CreatePullRequestPayload", (repo, pull))
        if field == "mergePullRequest":
            _, key = parse_global_id(input_data.get("pullRequestId") or "")
            parts = key.split(":")
            repo = gql_repo_by_dbid(int(parts[0])) if parts and parts[0].isdigit() else None
            pull = repo["pulls"].get(int(parts[1])) if repo and len(parts) > 1 else None
            if pull is None:
                raise GqlError("pull request not found")
            method = (input_data.get("mergeMethod") or "MERGE").lower()
            if merge_pull(repo, pull, method) is None:
                raise GqlError("pull request is not mergeable")
            return gql_tag("MergePullRequestPayload", (repo, pull))
        if field == "addComment":
            _, key = parse_global_id(input_data.get("subjectId") or "")
            parts = key.split(":")
            repo = gql_repo_by_dbid(int(parts[0])) if parts and parts[0].isdigit() else None
            issue = repo["issues"].get(int(parts[1])) if repo and len(parts) > 1 else None
            if issue is None:
                raise GqlError("subject not found")
            comment = {"id": FORGE.alloc_id(), "body": input_data.get("body") or "", "created_ts": CLOCK.tick()}
            issue["comments"].append(comment)
            return gql_tag("AddCommentPayload", (repo, issue, comment))
    if type_name == "User":
        return {"id": global_id("User", VIEWER_ID), "login": VIEWER_LOGIN, "name": VIEWER_NAME,
                "email": VIEWER_EMAIL, "url": ""}.get(field)
    if type_name == "Repository":
        repo = obj
        if field == "id":
            return global_id("Repository", repo["dbid"])
        if field == "databaseId":
            return repo["dbid"]
        if field == "name":
            return repo["name"]
        if field == "nameWithOwner":
            return "%s/%s" % (VIEWER_LOGIN, repo["name"])
        if field == "owner":
            return gql_tag("User", None)
        if field == "defaultBranchRef":
            return gql_tag("Ref", "main")
        if field == "viewerPermission":
            return "ADMIN"
        if field == "hasIssuesEnabled":
            return True
        if field == "hasPullRequestsEnabled":
            return True
        if field == "isPrivate":
            return repo["private"]
        if field == "url":
            return ""
        if field in ("createdAt", "updatedAt"):
            return iso_of(repo["created_ts"])
        if field == "issue":
            issue = repo["issues"].get(int(args.get("number", 0)))
            return gql_tag("Issue", (repo, issue)) if issue else None
        if field == "issues":
            return gql_issues_connection(repo, args)
        if field == "pullRequest":
            pull = repo["pulls"].get(int(args.get("number", 0)))
            return gql_tag("PullRequest", (repo, pull)) if pull else None
        if field == "pullRequests":
            return gql_pulls_connection(repo, args)
    if type_name == "Ref":
        if field == "name":
            return obj
        return None
    if type_name == "Issue":
        repo, issue = obj
        if field == "id":
            return global_id("Issue", "%d:%d" % (repo["dbid"], issue["number"]))
        if field == "number":
            return issue["number"]
        if field == "title":
            return issue["title"]
        if field == "body":
            return issue["body"]
        if field == "state":
            return issue["state"].upper()
        if field == "author":
            return gql_tag("User", None)
        if field == "createdAt":
            return iso_of(issue["created_ts"])
        if field == "updatedAt":
            return iso_of(issue["updated_ts"])
        if field == "url":
            return ""
        return None
    if type_name == "PullRequest":
        repo, pull = obj
        if field == "id":
            return global_id("PullRequest", "%d:%d" % (repo["dbid"], pull["number"]))
        if field == "number":
            return pull["number"]
        if field == "title":
            return pull["title"]
        if field == "body":
            return pull["body"]
        if field == "state":
            return "MERGED" if pull["merged"] else ("CLOSED" if pull["state"] == "closed" else "OPEN")
        if field == "merged":
            return pull["merged"]
        if field == "mergeable":
            return "CONFLICTING" if merge_conflicts(repo, pull) else "MERGEABLE"
        if field == "headRefName":
            return pull["head"]
        if field == "baseRefName":
            return pull["base"]
        if field == "author":
            return gql_tag("User", None)
        if field == "createdAt":
            return iso_of(pull["created_ts"])
        if field == "updatedAt":
            return iso_of(pull["updated_ts"])
        if field == "files":
            files = pull_files(repo, pull)
            tagged = [gql_tag("PullRequestChangedFile", f["filename"]) for f in files]
            return gql_tag("PullRequestChangedFileConnection", gql_connection(tagged, args))
    if type_name == "PullRequestChangedFile":
        if field == "path":
            return obj
        return None
    if type_name in ("IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection"):
        conn = obj
        if field == "nodes":
            return conn["items"]
        if field == "totalCount":
            return conn["total"]
        if field == "pageInfo":
            return gql_tag("PageInfo", conn["page"])
        return None
    if type_name == "PageInfo":
        return obj.get(field)
    if type_name in ("CreateIssuePayload", "CloseIssuePayload"):
        if field == "issue":
            return gql_tag("Issue", obj)
        return None
    if type_name in ("CreatePullRequestPayload", "MergePullRequestPayload"):
        if field == "pullRequest":
            return gql_tag("PullRequest", obj)
        return None
    if type_name == "AddCommentPayload":
        if field == "commentEdge":
            return gql_tag("IssueCommentEdge", obj)
        return None
    if type_name == "IssueCommentEdge":
        if field == "node":
            repo, issue, comment = obj
            return gql_tag("IssueComment", (repo, issue, comment))
        if field == "cursor":
            return base64.b64encode(b"cursor:0").decode()
        return None
    if type_name == "IssueComment":
        repo, issue, comment = obj
        if field == "id":
            return global_id("IssueComment", "%d:%d:%d" % (repo["dbid"], issue["number"], comment["id"]))
        if field == "body":
            return comment["body"]
        if field == "author":
            return gql_tag("User", None)
        if field == "createdAt":
            return iso_of(comment["created_ts"])
        return None
    if type_name == "__Type":
        if field == "name":
            return obj
        if field == "kind":
            return "ENUM" if obj in GQL_ENUM_VALUES else "OBJECT"
        if field == "description":
            return None
        if field == "fields":
            names = GQL_TYPE_FIELDS.get(obj)
            if names is None:
                return None
            return [gql_tag("__Field", name) for name in names]
        if field == "enumValues":
            values = GQL_ENUM_VALUES.get(obj)
            if values is None:
                return None
            return [gql_tag("__EnumValue", name) for name in values]
        return None
    if type_name == "__Field":
        if field == "name":
            return obj
        if field == "isDeprecated":
            return False
        return None
    if type_name == "__EnumValue":
        if field == "name":
            return obj
        if field == "isDeprecated":
            return False
        return None
    return None


def gql_execute_set(selections, type_name, obj, variables, fragments):
    out = {}
    for selection in selections:
        if selection["kind"] == "spread":
            fragment = fragments.get(selection["name"])
            if fragment:
                out.update(gql_execute_set(fragment["selections"], fragment["type"], obj, variables, fragments))
            continue
        if selection["kind"] == "inline":
            runtime = obj[1] if gql_is_tag(obj) else type_name
            if selection["type"] == runtime or selection["type"] == type_name:
                out.update(gql_execute_set(selection["selections"], selection["type"], obj, variables, fragments))
            continue
        name = selection["name"]
        key = selection["alias"] or name
        if name == "__typename":
            out[key] = obj[1] if gql_is_tag(obj) else type_name
            continue
        args = {arg_name: gql_resolve_value(arg_value, variables) for arg_name, arg_value in selection["args"].items()}
        payload = obj[2] if gql_is_tag(obj) else obj
        value = gql_resolve_field(type_name, payload, name, args)
        out[key] = gql_complete(value, selection, variables, fragments)
    return out


def gql_complete(value, selection, variables, fragments):
    if value is None:
        return None
    if gql_is_tag(value):
        if not selection["selections"]:
            return None
        return gql_execute_set(selection["selections"], value[1], value, variables, fragments)
    if isinstance(value, list):
        return [gql_complete(item, selection, variables, fragments) for item in value]
    return value


def gql_run_one(document):
    query = document.get("query")
    if not isinstance(query, str):
        return {"errors": [{"message": "query is required"}]}
    try:
        operations, fragments = GqlParser(query).parse_document()
    except GqlError as err:
        return {"errors": [{"message": str(err)}]}
    if not operations:
        return {"errors": [{"message": "no operation found"}]}
    operation_name = document.get("operationName")
    operation = operations[0]
    if operation_name:
        for candidate in operations:
            if candidate["name"] == operation_name:
                operation = candidate
    variables = {}
    for var_name, default in operation["var_defs"].items():
        if default is not None:
            variables[var_name] = gql_resolve_value(default, {})
    for var_name, value in (document.get("variables") or {}).items():
        variables[var_name] = value
    root_type = "Mutation" if operation["type"] == "mutation" else "Query"
    errors = []
    gql_validate(operation["selections"], root_type, fragments, errors)
    if errors:
        return {"errors": [{"message": message} for message in errors], "data": None}
    try:
        data = gql_execute_set(operation["selections"], root_type, None, variables, fragments)
    except GqlError as err:
        return {"errors": [{"message": str(err)}], "data": None}
    return {"data": data}


def graphql_reply(body):
    try:
        document = json.loads(body.decode("utf-8")) if body else None
    except Exception:
        return json_reply(400, {"errors": [{"message": "invalid JSON"}]})
    if isinstance(document, list):
        return json_reply(200, [gql_run_one(item) for item in document])
    if not isinstance(document, dict):
        return json_reply(400, {"errors": [{"message": "invalid request"}]})
    return json_reply(200, gql_run_one(document))


# --------------------------------------------------------------------------
# MCP over streamable HTTP
# --------------------------------------------------------------------------

MCP_SUPPORTED_VERSIONS = ["2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"]

MCP_TOOLS = [
    {
        "name": "create_repo",
        "description": "Create a new repository owned by the authenticated user.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "Repository name"},
                "description": {"type": "string", "description": "Repository description"},
                "private": {"type": "boolean", "description": "Whether the repository is private"},
                "owner": {"type": "string", "description": "Owner login (defaults to the viewer)"},
            },
            "required": ["name"],
            "additionalProperties": True,
        },
    },
    {
        "name": "get_repo",
        "description": "Read one repository by owner and name.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "owner": {"type": "string"},
                "repo": {"type": "string"},
            },
            "required": ["owner", "repo"],
            "additionalProperties": True,
        },
    },
    {
        "name": "list_repos",
        "description": "List the repositories owned by the authenticated user.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "owner": {"type": "string"},
            },
            "required": [],
            "additionalProperties": True,
        },
    },
    {
        "name": "create_issue",
        "description": "Create an issue in a repository.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "owner": {"type": "string"},
                "repo": {"type": "string"},
                "title": {"type": "string"},
                "body": {"type": "string"},
            },
            "required": ["owner", "repo", "title"],
            "additionalProperties": True,
        },
    },
    {
        "name": "get_issue",
        "description": "Read one issue by number.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "owner": {"type": "string"},
                "repo": {"type": "string"},
                "issue_number": {"type": "integer"},
            },
            "required": ["owner", "repo", "issue_number"],
            "additionalProperties": True,
        },
    },
    {
        "name": "list_issues",
        "description": "List issues in a repository, optionally filtered by state.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "owner": {"type": "string"},
                "repo": {"type": "string"},
                "state": {"type": "string"},
            },
            "required": ["owner", "repo"],
            "additionalProperties": True,
        },
    },
    {
        "name": "create_pull_request",
        "description": "Open a pull request from a head branch into a base branch.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "owner": {"type": "string"},
                "repo": {"type": "string"},
                "title": {"type": "string"},
                "body": {"type": "string"},
                "head": {"type": "string"},
                "base": {"type": "string"},
            },
            "required": ["owner", "repo", "title", "head", "base"],
            "additionalProperties": True,
        },
    },
    {
        "name": "merge_pull_request",
        "description": "Merge an open pull request.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "owner": {"type": "string"},
                "repo": {"type": "string"},
                "pull_number": {"type": "integer"},
            },
            "required": ["owner", "repo", "pull_number"],
            "additionalProperties": True,
        },
    },
]

MCP_TOOLS_BY_NAME = {tool["name"]: tool for tool in MCP_TOOLS}


def mcp_tool_result(obj):
    return {
        "content": [{"type": "text", "text": json.dumps(obj)}],
        "structuredContent": obj,
        "isError": False,
    }


def mcp_tool_error(message):
    return {
        "content": [{"type": "text", "text": message}],
        "isError": True,
    }


def mcp_call_tool(name, arguments):
    tool = MCP_TOOLS_BY_NAME.get(name)
    if tool is None:
        return None, {"code": -32602, "message": "unknown tool: %s" % name}
    required = tool["inputSchema"].get("required", [])
    missing = [key for key in required if arguments.get(key) is None]
    if missing:
        return None, {"code": -32602, "message": "missing required arguments: %s" % ", ".join(missing)}
    if name == "create_repo":
        repo_name = arguments["name"]
        if repo_name in FORGE.repos:
            return mcp_tool_error("repository already exists"), None
        repo = FORGE.create_repo(repo_name, arguments.get("private", False), arguments.get("description", ""))
        return mcp_tool_result(repo_json(repo)), None
    if name == "list_repos":
        return mcp_tool_result([repo_json(r) for r in FORGE.repos.values()]), None
    repo = FORGE.get_repo(arguments.get("owner"), arguments.get("repo"))
    if repo is None:
        return mcp_tool_error("repository not found"), None
    if name == "get_repo":
        return mcp_tool_result(repo_json(repo)), None
    if name == "create_issue":
        issue = create_issue(repo, arguments.get("title"), arguments.get("body"), [])
        return mcp_tool_result(issue_json(repo, issue)), None
    if name == "get_issue":
        issue = repo["issues"].get(int(arguments["issue_number"]))
        if issue is None:
            return mcp_tool_error("issue not found"), None
        return mcp_tool_result(issue_json(repo, issue)), None
    if name == "list_issues":
        state = arguments.get("state") or "open"
        items = [issue_json(repo, i) for i in repo["issues"].values() if state == "all" or i["state"] == state]
        items.sort(key=lambda i: i["number"])
        return mcp_tool_result(items), None
    if name == "create_pull_request":
        head = str(arguments["head"]).split(":")[-1]
        base = str(arguments["base"]).split(":")[-1]
        if branch_tip(repo, head) is None or branch_tip(repo, base) is None:
            return mcp_tool_error("head or base branch does not exist"), None
        pull = create_pull(repo, arguments.get("title"), arguments.get("body"), head, base)
        return mcp_tool_result(pull_json(repo, pull)), None
    if name == "merge_pull_request":
        pull = repo["pulls"].get(int(arguments["pull_number"]))
        if pull is None:
            return mcp_tool_error("pull request not found"), None
        if merge_pull(repo, pull, "merge") is None:
            return mcp_tool_error("pull request is not mergeable"), None
        return mcp_tool_result(pull_json(repo, pull)), None
    return None, {"code": -32602, "message": "unknown tool: %s" % name}


def mcp_reply(body):
    try:
        request = json.loads(body.decode("utf-8")) if body else None
    except Exception:
        return json_reply(400, {"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "Parse error"}})
    if isinstance(request, list):
        responses = []
        for item in request:
            reply = mcp_handle_one(item)
            if reply is not None:
                responses.append(reply)
        return json_reply(200, responses)
    reply = mcp_handle_one(request)
    if reply is None:
        return Reply(202, b"", "application/json; charset=utf-8")
    return json_reply(200, reply)


def mcp_handle_one(request):
    if not isinstance(request, dict):
        return {"jsonrpc": "2.0", "id": None, "error": {"code": -32600, "message": "Invalid Request"}}
    method = request.get("method")
    request_id = request.get("id")
    params = request.get("params") or {}
    if request_id is None and isinstance(method, str) and method.startswith("notifications/"):
        return None

    def result(value):
        return {"jsonrpc": "2.0", "id": request_id, "result": value}

    def error(code, message):
        return {"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": message}}

    if method == "server/discover":
        return result({
            "serverInfo": {"name": "forge-mcp", "version": FORGE_VERSION},
            "capabilities": {"tools": {}},
            "supportedVersions": MCP_SUPPORTED_VERSIONS,
        })
    if method == "initialize":
        offered = params.get("protocolVersion")
        chosen = offered if offered in MCP_SUPPORTED_VERSIONS else "2025-11-25"
        return result({
            "protocolVersion": chosen,
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "forge-mcp", "version": FORGE_VERSION},
        })
    if method == "ping":
        return result({})
    if method == "tools/list":
        return result({"tools": MCP_TOOLS})
    if method == "tools/call":
        tool_name = params.get("name", "")
        arguments = params.get("arguments") or {}
        value, err = mcp_call_tool(tool_name, arguments)
        if err is not None:
            return error(err["code"], err["message"])
        return result(value)
    return error(-32601, "Method not found: %s" % method)


# --------------------------------------------------------------------------
# Git smart HTTP
# --------------------------------------------------------------------------

def pkt_line(payload):
    return ("%04x" % (len(payload) + 4)).encode() + payload


PKT_FLUSH = b"0000"
PKT_DELIM = b"0001"


def iter_pkt_lines(data, offset=0):
    while offset + 4 <= len(data):
        try:
            length = int(data[offset:offset + 4].decode("ascii"), 16)
        except Exception:
            return
        if length == 0:
            yield ("flush", b"")
            offset += 4
        elif length == 1:
            yield ("delim", b"")
            offset += 4
        elif length == 2:
            yield ("response-end", b"")
            offset += 4
        elif length == 3:
            yield ("error", b"")
            offset += 4
        else:
            yield ("data", data[offset + 4:offset + length])
            offset += length
    return


def sorted_ref_items(repo):
    return sorted(repo["refs"].items())


def info_refs_reply(repo, service, version2):
    if service == "git-upload-pack" and version2:
        lines = [pkt_line(b"# service=git-upload-pack\n"), PKT_FLUSH,
                 pkt_line(b"version 2\n"),
                 pkt_line(b"agent=forge/" + FORGE_VERSION.encode() + b"\n"),
                 pkt_line(b"ls-refs=unborn\n"),
                 pkt_line(b"fetch=shallow wait-for-done\n"),
                 pkt_line(b"server-option\n"),
                 pkt_line(b"object-format=sha1\n"),
                 PKT_FLUSH]
        return Reply(200, b"".join(lines), "application/x-git-upload-pack-advertisement")
    if service == "git-upload-pack":
        capabilities = ("multi_ack_detailed no-done side-band-64k thin-pack ofs-delta "
                        "deepen-since deepen-not symref=HEAD:refs/heads/main agent=forge/" + FORGE_VERSION)
    else:
        capabilities = "report-status side-band-64k delete-refs ofs-delta agent=forge/" + FORGE_VERSION
    body = [pkt_line(("# service=%s\n" % service).encode()), PKT_FLUSH]
    refs = sorted_ref_items(repo)
    head_oid = repo["refs"].get("refs/heads/main")
    if refs:
        first = True
        if head_oid:
            body.append(pkt_line(("%s HEAD\0%s\n" % (head_oid, capabilities)).encode()))
            first = False
        for ref, oid in refs:
            suffix = ("\0" + capabilities) if first else ""
            body.append(pkt_line(("%s %s%s\n" % (oid, ref, suffix)).encode()))
            first = False
            if ref.startswith("refs/tags/"):
                otype, _ = STORE.get(oid) if STORE.has(oid) else (None, None)
                if otype == "tag":
                    _, tag = STORE.get(oid)
                    target = parse_tag_target(tag)
                    if target:
                        body.append(pkt_line(("%s %s^{}\n" % (target, ref)).encode()))
    else:
        body.append(pkt_line(("%s capabilities^{}\0%s\n" % (ZERO_OID, capabilities)).encode()))
    body.append(PKT_FLUSH)
    return Reply(200, b"".join(body), "application/x-%s-advertisement" % service)


def parse_tag_target(data):
    for line in data.split(b"\n"):
        if line.startswith(b"object "):
            return line[7:].decode().strip()
    return None


def sideband_packets(pack):
    out = []
    step = 32768
    for index in range(0, len(pack), step):
        out.append(pkt_line(b"\x01" + pack[index:index + step]))
    return b"".join(out)


def ls_refs_reply(repo, args):
    prefixes = [a[len("ref-prefix "):] for a in args if a.startswith("ref-prefix ")]
    want_peel = "peel" in args
    want_symrefs = "symrefs" in args
    want_unborn = "unborn" in args
    lines = []
    head_oid = repo["refs"].get("refs/heads/main")

    def prefix_ok(ref):
        return not prefixes or any(ref.startswith(p) for p in prefixes)

    if want_symrefs or want_unborn:
        if prefix_ok("HEAD"):
            if head_oid:
                lines.append("%s HEAD symref-target:refs/heads/main" % head_oid)
            elif want_unborn:
                lines.append("%s HEAD symref-target:refs/heads/main" % "unborn")
    for ref, oid in sorted_ref_items(repo):
        if not prefix_ok(ref):
            continue
        line = "%s %s" % (oid, ref)
        if want_peel and ref.startswith("refs/tags/") and STORE.has(oid):
            otype, _ = STORE.get(oid)
            if otype == "tag":
                _, tag = STORE.get(oid)
                target = parse_tag_target(tag)
                if target:
                    line += " peeled:%s" % target
        lines.append(line)
    body = b"".join(pkt_line((line + "\n").encode()) for line in lines) + PKT_FLUSH
    return Reply(200, body, "application/x-git-upload-pack-result")


def fetch_reply(repo, wants, haves, shallow, depth, since, version2, done=True):
    have_set = set(haves)
    common = [h for h in haves if STORE.has(h) and STORE.type_of(h) == "commit"]
    cache_key = (repo["name"], tuple(sorted(wants)), tuple(sorted(have_set)),
                 tuple(sorted(shallow)), depth, since)
    cached = FORGE.pack_cache.get(cache_key)
    if cached is not None:
        pack, new_shallow = cached
    else:
        objects, boundary = pack_for_fetch(wants, have_set, set(shallow), depth, since)
        pack = write_pack(objects) if objects else None
        new_shallow = set(boundary)
        FORGE.pack_cache[cache_key] = (pack, new_shallow)
    needs_pack = pack is not None
    if version2:
        sections = []
        if not needs_pack:
            sections.append(pkt_line(b"acknowledgments\n") + pkt_line(b"NAK\n") + PKT_FLUSH)
            return Reply(200, b"".join(sections), "application/x-git-upload-pack-result")
        if new_shallow:
            info = [pkt_line(b"shallow-info\n")]
            for oid in sorted(new_shallow):
                info.append(pkt_line(("shallow %s\n" % oid).encode()))
            for oid in sorted(set(shallow) - new_shallow):
                info.append(pkt_line(("unshallow %s\n" % oid).encode()))
            info.append(PKT_FLUSH)
            sections.append(b"".join(info))
        sections.append(pkt_line(b"packfile\n") + sideband_packets(pack) + PKT_FLUSH)
        return Reply(200, b"".join(sections), "application/x-git-upload-pack-result")
    lines = []
    for oid in sorted(new_shallow):
        lines.append(pkt_line(("shallow %s\n" % oid).encode()))
    for oid in sorted(set(shallow) - new_shallow):
        lines.append(pkt_line(("unshallow %s\n" % oid).encode()))
    for oid in common:
        lines.append(pkt_line(("ACK %s common\n" % oid).encode()))
    if common:
        lines.append(pkt_line(("ACK %s\n" % common[-1]).encode()))
    else:
        lines.append(pkt_line(b"NAK\n"))
    body = b"".join(lines)
    if needs_pack:
        body += sideband_packets(pack)
    body += PKT_FLUSH
    return Reply(200, body, "application/x-git-upload-pack-result")


def upload_pack_reply(repo, body, version2):
    if version2:
        command = None
        args = []
        phase = "command"
        for kind, payload in iter_pkt_lines(body):
            if kind == "delim":
                phase = "args"
                continue
            if kind == "flush":
                break
            if kind != "data":
                continue
            line = payload.decode("utf-8", "replace").rstrip("\n")
            if phase == "command":
                if line.startswith("command="):
                    command = line[len("command="):]
            else:
                args.append(line)
        if command == "ls-refs":
            return ls_refs_reply(repo, args)
        if command == "fetch":
            wants, haves, shallow = [], [], []
            depth = None
            since = None
            done = False
            for line in args:
                if line.startswith("want "):
                    wants.append(line[5:].strip())
                elif line.startswith("have "):
                    haves.append(line[5:].strip())
                elif line.startswith("shallow "):
                    shallow.append(line[8:].strip())
                elif line.startswith("deepen "):
                    depth = int(line[7:].strip())
                elif line.startswith("deepen-since "):
                    since = int(line[13:].strip())
                elif line == "done":
                    done = True
            if not done:
                return Reply(200, pkt_line(b"acknowledgments\n") + pkt_line(b"NAK\n") + PKT_FLUSH,
                             "application/x-git-upload-pack-result")
            return fetch_reply(repo, wants, haves, shallow, depth, since, True)
        return Reply(400, b"unsupported command", "text/plain")
    wants, haves, shallow = [], [], []
    depth = None
    since = None
    section = 0
    for kind, payload in iter_pkt_lines(body):
        if kind == "flush":
            section += 1
            continue
        if kind != "data":
            continue
        line = payload.decode("utf-8", "replace").rstrip("\n")
        if line.startswith("want "):
            wants.append(line[5:].split(" ")[0])
        elif line.startswith("have "):
            haves.append(line[5:].strip())
        elif line.startswith("shallow "):
            shallow.append(line[8:].strip())
        elif line.startswith("deepen "):
            depth = int(line[7:].strip())
        elif line.startswith("deepen-since "):
            since = int(line[13:].strip())
    return fetch_reply(repo, wants, haves, shallow, depth, since, False)


def receive_pack_reply(repo, body):
    commands = []
    capabilities = ""
    offset = 0
    for kind, payload in iter_pkt_lines(body):
        offset += 4 if kind != "data" else 4 + len(payload)
        if kind == "flush":
            break
        if kind != "data":
            continue
        line = payload.decode("utf-8", "replace").rstrip("\n")
        if "\0" in line:
            line, capabilities = line.split("\0", 1)
        parts = line.split(" ")
        if len(parts) == 3:
            commands.append((parts[0], parts[1], parts[2]))
    pack = body[offset:]
    unpack_ok = True
    if pack:
        try:
            read_pack(pack)
        except Exception:
            unpack_ok = False
    results = []
    for old, new, ref in commands:
        reason = None
        if not (ref.startswith("refs/heads/") or ref.startswith("refs/tags/")):
            reason = "invalid ref"
        else:
            current = repo["refs"].get(ref, ZERO_OID)
            if old != current:
                reason = "stale info"
            elif new != ZERO_OID:
                if not STORE.has(new):
                    reason = "missing object"
                elif ref.startswith("refs/heads/"):
                    if STORE.type_of(new) != "commit":
                        reason = "not a commit"
                    elif old != ZERO_OID and old not in ancestors_of(new):
                        reason = "non-fast-forward"
            if reason is None and not unpack_ok and new != ZERO_OID:
                reason = "unpack failed"
        if reason is None:
            if new == ZERO_OID:
                repo["refs"].pop(ref, None)
            else:
                repo["refs"][ref] = new
            results.append("ok %s" % ref)
        else:
            results.append("ng %s %s" % (ref, reason))
    FORGE.pack_cache.clear()
    report_lines = ["unpack ok" if unpack_ok else "unpack error"]
    report_lines.extend(results)
    report = "".join(line + "\n" for line in report_lines).encode()
    if "side-band-64k" in capabilities:
        payload = sideband_packets(report) + PKT_FLUSH
    else:
        payload = b"".join(pkt_line(line) for line in report.splitlines(keepends=True)) + PKT_FLUSH
        if not report:
            payload = PKT_FLUSH
    return Reply(200, payload, "application/x-git-receive-pack-result")


# --------------------------------------------------------------------------
# HTTP server and trial lifecycle
# --------------------------------------------------------------------------

REQUEST_COUNTER = {"value": 0}


def expected_token():
    return os.environ.get("MAKEFUN_SHOP_TOKEN", "")


def is_authorized(handler):
    token = expected_token()
    if not token:
        return True
    return handler.headers.get("Authorization") == "Bearer " + token


class ForgeHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "Forge/1.0"

    def log_message(self, fmt, *args):
        pass

    def setup(self):
        super().setup()
        try:
            self.request.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        except OSError:
            pass

    def do_GET(self):
        self.handle_all()

    def do_POST(self):
        self.handle_all()

    def do_PUT(self):
        self.handle_all()

    def do_PATCH(self):
        self.handle_all()

    def do_DELETE(self):
        self.handle_all()

    def read_body(self):
        length = self.headers.get("Content-Length")
        if length is None:
            return b""
        try:
            size = int(length)
        except ValueError:
            return b""
        return self.rfile.read(size) if size > 0 else b""

    def handle_all(self):
        try:
            reply = self.dispatch()
        except Exception as err:  # never hang the suite on an internal error
            reply = json_reply(500, {"message": "Internal error: %s" % err})
        self.send_reply(reply)

    def dispatch(self):
        parsed = urlparse(self.path)
        path = parsed.path
        query = parse_qs(parsed.query)
        segments = [unquote(part) for part in path.split("/") if part != ""]
        host = self.headers.get("Host", "127.0.0.1")
        body = self.read_body() if self.command in ("POST", "PUT", "PATCH", "DELETE") else b""
        if path == "/healthz":
            return json_reply(200, {"status": "ok"})
        if segments[:2] == ["api", "v3"]:
            if not is_authorized(self):
                return json_reply(401, {"message": "Bad credentials"})
            with LOCK:
                return rest_reply(self.command, segments[2:], query, body, host, path)
        if segments == ["api", "graphql"]:
            if self.command != "POST":
                return not_found()
            if not is_authorized(self):
                return json_reply(401, {"message": "Bad credentials"})
            with LOCK:
                return graphql_reply(body)
        if segments == ["mcp"]:
            if self.command != "POST":
                return not_found()
            if not is_authorized(self):
                return json_reply(401, {"jsonrpc": "2.0", "id": None,
                                        "error": {"code": -32001, "message": "Unauthorized"}})
            with LOCK:
                return mcp_reply(body)
        if len(segments) >= 3 and segments[1].endswith(".git"):
            return self.dispatch_git(segments, query, body)
        return not_found()

    def dispatch_git(self, segments, query, body):
        owner = segments[0]
        repo_name = segments[1][:-4]
        tail = segments[2:]
        if not is_authorized(self):
            return json_reply(401, {"message": "Bad credentials"})
        repo = FORGE.get_repo(owner, repo_name)
        if repo is None:
            return not_found()
        protocol = self.headers.get("Git-Protocol", "")
        version2 = "version=2" in protocol
        if tail == ["info", "refs"] and self.command == "GET":
            service = query.get("service", [""])[0]
            if service not in ("git-upload-pack", "git-receive-pack"):
                return Reply(400, b"unsupported service", "text/plain")
            with LOCK:
                return info_refs_reply(repo, service, version2)
        if tail == ["git-upload-pack"] and self.command == "POST":
            with LOCK:
                return upload_pack_reply(repo, body, version2)
        if tail == ["git-receive-pack"] and self.command == "POST":
            with LOCK:
                return receive_pack_reply(repo, body)
        return not_found()

    def send_reply(self, reply):
        REQUEST_COUNTER["value"] += 1
        reason = self.responses.get(reply.status, ("Reply",))[0]
        lines = ["HTTP/1.1 %d %s\r\n" % (reply.status, reason),
                 "Content-Type: %s\r\n" % reply.content_type,
                 "Content-Length: %d\r\n" % len(reply.body),
                 "X-GitHub-Request-Id: req-%d\r\n" % REQUEST_COUNTER["value"]]
        for key, value in reply.headers.items():
            lines.append("%s: %s\r\n" % (key, value))
        head = "".join(lines).encode() + b"\r\n"
        try:
            self.wfile.write(head + reply.body)
        except (BrokenPipeError, ConnectionResetError):
            pass


class ForgeServer(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 512
    allow_reuse_address = True


def env_post_json(url, payload):
    request = urllib.request.Request(
        url, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(request, timeout=5) as response:
        return response.status


def env_get_json(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        return json.loads(response.read().decode())


def main():
    env_url = os.environ.get("MAKEFUN_ENV_URL", "").rstrip("/")
    if env_url:
        server = ForgeServer(("0.0.0.0", 0), ForgeHandler)
    else:
        server = ForgeServer(("127.0.0.1", int(os.environ.get("PORT", "0"))), ForgeHandler)
    port = server.server_address[1]
    if not env_url:
        print("forge listening on 127.0.0.1:%d" % port, file=sys.stderr)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
        return 0
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    registered = False
    for _ in range(120):
        try:
            env_post_json(env_url + "/register", {"port": port})
            registered = True
            break
        except urllib.error.HTTPError as err:
            if err.code == 409:
                registered = True
                break
            time.sleep(0.25)
        except Exception:
            time.sleep(0.25)
    if not registered:
        print("warning: could not register port with env", file=sys.stderr)
    delay = 0.2
    while True:
        try:
            state = env_get_json(env_url + "/done")
            if state.get("done"):
                break
            delay = 0.2
        except Exception:
            delay = min(delay * 1.5, 2.0)
        time.sleep(delay)
    server.shutdown()
    server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
