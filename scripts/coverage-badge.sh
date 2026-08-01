#!/usr/bin/env bash
# Regenerate coverage.svg from the current test coverage. The badge is committed
# so the README renders it without a third-party service, and CI fails if it
# drifts from what the tests actually cover.
set -euo pipefail

cd "$(dirname "$0")/.."

profile="$(mktemp)"
trap 'rm -f "$profile"' EXIT

go test ./... -coverprofile="$profile" >/dev/null
pct="$(go tool cover -func="$profile" | tail -1 | awk '{print $3}' | tr -d '%')"

# Same thresholds shields.io uses, so the colour means what people expect.
if   awk "BEGIN{exit !($pct >= 90)}"; then colour="#4c1"
elif awk "BEGIN{exit !($pct >= 75)}"; then colour="#97ca00"
elif awk "BEGIN{exit !($pct >= 60)}"; then colour="#a4a61d"
elif awk "BEGIN{exit !($pct >= 40)}"; then colour="#dfb317"
elif awk "BEGIN{exit !($pct >= 20)}"; then colour="#fe7d37"
else                                       colour="#e05d44"
fi

label="coverage"
value="${pct}%"
# 7px per character plus padding is close enough to Verdana 11 for these widths.
label_w=$(( ${#label} * 7 + 10 ))
value_w=$(( ${#value} * 7 + 10 ))
total_w=$(( label_w + value_w ))

cat > coverage.svg <<SVG
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="${total_w}" height="20" role="img" aria-label="${label}: ${value}">
  <title>${label}: ${value}</title>
  <linearGradient id="s" x2="0" y2="100%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="${total_w}" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="${label_w}" height="20" fill="#555"/>
    <rect x="${label_w}" width="${value_w}" height="20" fill="${colour}"/>
    <rect width="${total_w}" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="$(( label_w * 5 ))" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="$(( (label_w - 10) * 10 ))">${label}</text>
    <text x="$(( label_w * 5 ))" y="140" transform="scale(.1)" textLength="$(( (label_w - 10) * 10 ))">${label}</text>
    <text x="$(( (label_w * 10 + value_w * 5) ))" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="$(( (value_w - 10) * 10 ))">${value}</text>
    <text x="$(( (label_w * 10 + value_w * 5) ))" y="140" transform="scale(.1)" textLength="$(( (value_w - 10) * 10 ))">${value}</text>
  </g>
</svg>
SVG

echo "coverage.svg: ${value}"
