#!/usr/bin/env zsh
# merge_vcf.zsh — deduplicate and merge multiple VCF files into one.
# For each unique EMAIL address, keeps the record with the most populated fields.
# Records with no EMAIL are retained as-is (no deduplication possible).
#
# Usage: merge_vcf.zsh [-o output.vcf] input.vcf [input.vcf ...]

set -euo pipefail

usage() {
    print "Usage: ${0:t} [-o FILE] <input.vcf> [input.vcf ...]"
    print "  -o, --output FILE   output path  (default: merged.vcf)"
    print "  -h, --help          show this help"
}

# ── Argument parsing ──────────────────────────────────────────────────────────

output=merged.vcf
typeset -a inputs

while (( $# )); do
    case $1 in
        -o|--output) output=$2;          shift 2 ;;
        -h|--help)   usage;              exit 0  ;;
        --)          shift; inputs+=("$@"); break ;;
        -*)          print -u2 "Unknown option: $1"; usage >&2; exit 1 ;;
        *)           inputs+=($1);       shift   ;;
    esac
done

(( ${#inputs} )) || { print -u2 "error: no input files given"; usage >&2; exit 1 }

for f in $inputs; do
    [[ -f $f ]] || { print -u2 "error: file not found: $f"; exit 1 }
done

# ── Core dedup via awk ────────────────────────────────────────────────────────
# Reads all VCARD blocks across every input file.
# Dedup key   : lower-cased EMAIL value (case-insensitive, whitespace stripped).
# Scoring     : one point per populated non-structural field (FN, ORG, SOURCE, …).
#               Higher score wins; equal score keeps the first-seen record.
# Output      : CRLF line endings as required by RFC 6350.

awk '
BEGIN { block=""; key=""; score=0; total=0; nomail=0 }

# Normalise Windows CRLF → LF on the way in.
{ sub(/\r$/, "") }

/^[Bb][Ee][Gg][Ii][Nn]:[Vv][Cc][Aa][Rr][Dd]/ {
    block=$0"\n"; key=""; score=0
    next
}

/^[Ee][Nn][Dd]:[Vv][Cc][Aa][Rr][Dd]/ {
    block=block $0"\n"
    total++
    # Records without an email get a unique synthetic key so they are kept.
    if (key == "") key="__nomail_" nomail++
    if (!(key in best_score) || score > best_score[key]) {
        best_score[key] = score
        best_block[key] = block
    }
    block=""; key=""; score=0
    next
}

# Accumulate lines belonging to the current block.
block != "" {
    block=block $0"\n"

    colon=index($0,":")
    if (colon < 2) next

    field = tolower(substr($0, 1, colon-1))
    val   = substr($0, colon+1)
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", val)

    if (field == "email") {
        key = tolower(val)
        gsub(/[[:space:]]/, "", key)
    }

    # Score every populated field except structural ones.
    # Primary: +10000 per field so more fields always beats fewer fields.
    # Secondary: +length(val) so richer values break ties between equal field counts.
    if (field != "begin" && field != "end" && field != "version" && length(val) > 0)
        score += 10000 + length(val)
}

END {
    written=0
    for (k in best_block) {
        # Re-emit with CRLF line endings (RFC 6350 §3.2).
        n = split(best_block[k], lines, "\n")
        for (i=1; i<=n; i++)
            if (lines[i] != "") printf "%s\r\n", lines[i]
        written++
    }
    print total " records read, " written " unique contacts written." > "/dev/stderr"
}
' "${inputs[@]}" > "$output"

print "Done → $output"
