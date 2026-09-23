#!/bin/sh
# install-firmware.sh - install or enforce the SaviorOS firmware allowlist.
#
#   install-firmware.sh SRC_DIR DEST_DIR LIST_FILE
#       Copy the files LIST_FILE selects from a linux-firmware source tree
#       (SRC_DIR, with its WHENCE file) into DEST_DIR (the target's
#       /lib/firmware). WHENCE "Link:" entries are handled like upstream's
#       copy-firmware.sh: a selected link name installs its target, and every
#       link whose target got installed is created.
#   install-firmware.sh --prune DIR LIST_FILE
#       Delete every file in DIR the list doesn't select (plus dangling links
#       and empty directories). Keeps regulatory.db* (wireless-regdb).
#
# LIST_FILE syntax: see board/savior/firmware.list. Exit status 1 when a
# required pattern matches nothing (upstream renamed something: fix the list).
set -eu

die() { echo "install-firmware: error: $*" >&2; exit 1; }
log() { echo "install-firmware: $*" >&2; }

PRUNE=no
if [ "${1:-}" = "--prune" ]; then
	PRUNE=yes
	shift
	[ $# -eq 2 ] || die "usage: install-firmware.sh --prune DIR LIST_FILE"
	SRC=$1; DEST=$1; LIST=$2
else
	[ $# -eq 3 ] || die "usage: install-firmware.sh SRC_DIR DEST_DIR LIST_FILE"
	SRC=$1; DEST=$2; LIST=$3
fi
[ -d "$SRC" ] || die "not a directory: $SRC"
[ -f "$LIST" ] || die "list not found: $LIST"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/savior-fw.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM

# Inventory: regular files and links, as paths relative to SRC.
(cd "$SRC" && find . -type f) | sed 's#^\./##' | LC_ALL=C sort >"$WORK/files"
if [ "$PRUNE" = yes ]; then
	# Links as they exist on disk: "name<TAB>target".
	(cd "$SRC" && find . -type l) | sed 's#^\./##' | while IFS= read -r l; do
		printf '%s\t%s\n' "$l" "$(readlink "$SRC/$l")"
	done >"$WORK/links"
else
	[ -f "$SRC/WHENCE" ] || die "$SRC/WHENCE not found (not a linux-firmware tree?)"
	grep -v -x -e WHENCE "$WORK/files" >"$WORK/files.tmp" || true
	mv "$WORK/files.tmp" "$WORK/files"
	sed -n 's/^Link: \(.*\) -> \(.*\)$/\1	\2/p' "$SRC/WHENCE" | tr -d '\r' >"$WORK/links"
fi

# Selection. Output lines: "F path" (file to keep/install), "L name<TAB>target"
# (link to create), "E pattern" (required pattern without a match).
LC_ALL=C awk -v prune="$PRUNE" -v listfile="$LIST" -v workdir="$WORK" '
function glob2re(g,    re, i, c, n) {
	re = "^"; n = length(g)
	for (i = 1; i <= n; i++) {
		c = substr(g, i, 1)
		if (c == "*") re = re ".*"
		else if (c == "?") re = re "."
		else if (c == "[") {
			j = index(substr(g, i + 1), "]")
			if (j == 0) { re = re "\\["; continue }
			cls = substr(g, i + 1, j - 1)
			if (substr(cls, 1, 1) == "!") cls = "^" substr(cls, 2)
			re = re "[" cls "]"; i += j
		} else if (index("\\^$.|+(){}", c)) re = re "\\" c
		else re = re c
	}
	return re "$"
}
function normalize(p,    n, parts, out, k, i) {
	n = split(p, parts, "/"); k = 0
	for (i = 1; i <= n; i++) {
		if (parts[i] == "" || parts[i] == ".") continue
		if (parts[i] == "..") { if (k > 0) k--; continue }
		out[++k] = parts[i]
	}
	p = ""
	for (i = 1; i <= k; i++) p = p (i > 1 ? "/" : "") out[i]
	return p
}
function dirof(p) { return (p ~ /\//) ? substr(p, 1, match(p, /\/[^\/]*$/) - 1) : "" }
function resolve(name,    t, depth) {
	# Follow link chains to a regular file; "" if none.
	for (depth = 0; depth < 16; depth++) {
		if (name in isfile) return name
		if (!(name in linkto)) return ""
		t = linkto[name]
		name = normalize((substr(t, 1, 1) == "/" ? "" : dirof(name) "/") t)
	}
	return ""
}
function selected(p,    i, hit) {
	hit = 0
	for (i = 1; i <= ninc; i++) if (p ~ inc[i]) { hit = 1; used[i] = 1 }
	if (!hit) return 0
	for (i = 1; i <= nexc; i++) if (p ~ exc[i]) return 0
	return 1
}
BEGIN {
	while ((getline line < listfile) > 0) {
		sub(/\r$/, "", line); sub(/^[ \t]+/, "", line); sub(/[ \t]+$/, "", line)
		if (line == "" || line ~ /^#/) continue
		if (substr(line, 1, 1) == "!") { exc[++nexc] = glob2re(substr(line, 2)); continue }
		opt = 0
		if (substr(line, 1, 1) == "?") { opt = 1; line = substr(line, 2) }
		inc[++ninc] = glob2re(line); incsrc[ninc] = line; incopt[ninc] = opt
	}
	close(listfile)
	while ((getline line < (workdir "/files")) > 0) isfile[line] = 1
	close(workdir "/files")
	while ((getline line < (workdir "/links")) > 0) {
		t = index(line, "\t"); if (!t) continue
		name = substr(line, 1, t - 1); linkto[name] = substr(line, t + 1)
	}
	close(workdir "/links")

	for (f in isfile) if (selected(f)) keep[f] = 1
	for (l in linkto) if (selected(l)) { r = resolve(l); if (r != "") keep[r] = 1 }
	if (prune == "yes") for (f in isfile) if (f ~ /^regulatory\.db/) keep[f] = 1

	for (f in keep) print "F " f
	for (l in linkto) { r = resolve(l); if (r != "" && (r in keep)) print "L " l "\t" linkto[l] }
	for (i = 1; i <= ninc; i++) if (!used[i] && !incopt[i]) print "E " incsrc[i]
}' >"$WORK/plan"

# Pruning is a safety net over whatever is installed (possibly nothing, on a
# re-run): only a real install must find every required pattern.
if [ "$PRUNE" = no ] && grep -q '^E ' "$WORK/plan"; then
	sed -n 's/^E /  no match: /p' "$WORK/plan" >&2
	die "firmware list patterns matched nothing in $SRC (update $LIST)"
fi
sed -n 's/^F //p' "$WORK/plan" | LC_ALL=C sort >"$WORK/keep"

if [ "$PRUNE" = yes ]; then
	removed=0
	LC_ALL=C comm -23 "$WORK/files" "$WORK/keep" | while IFS= read -r f; do
		rm -f "$DEST/$f"
		echo x
	done >"$WORK/removed"
	removed=$(wc -l <"$WORK/removed" | tr -d ' ')
	# Dangling links, then empty directories.
	find "$DEST" -type l | while IFS= read -r l; do
		[ -e "$l" ] || rm -f "$l"
	done
	find "$DEST" -depth -mindepth 1 -type d -exec rmdir {} \; 2>/dev/null || true
	log "pruned $removed file(s) not on the allowlist from $DEST"
	exit 0
fi

mkdir -p "$DEST"
n=0
while IFS= read -r f; do
	mkdir -p "$DEST/$(dirname "$f")"
	rm -f "$DEST/$f"
	cp "$SRC/$f" "$DEST/$f"
	chmod 0644 "$DEST/$f"
	n=$((n + 1))
done <"$WORK/keep"
nl=0
sed -n 's/^L //p' "$WORK/plan" | LC_ALL=C sort | while IFS="	" read -r name target; do
	mkdir -p "$DEST/$(dirname "$name")"
	# A link must never replace an installed file of the same name.
	[ -f "$DEST/$name" ] && [ ! -L "$DEST/$name" ] && continue
	ln -sfn "$target" "$DEST/$name"
	echo x
done >"$WORK/linked"
nl=$(wc -l <"$WORK/linked" | tr -d ' ')
log "installed $n file(s) and $nl link(s) into $DEST ($(du -sk "$DEST" | cut -f1) KiB)"
