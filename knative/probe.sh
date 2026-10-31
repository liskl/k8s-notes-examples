#!/bin/sh
# probe.sh URL SECONDS: ask URL every 0.2 s for SECONDS, then say how many answers were 200, and
# when the others came (the first and the last). Field Guide 05 runs it in the background while
# the lab changes under a Service.
url=$1
end=$(( $(date +%s) + $2 ))
ok=0 bad=0 first= last=
while [ "$(date +%s)" -lt "$end" ]; do
  code=$(curl -s -o /dev/null -m 2 -w '%{http_code}' "$url")
  if [ "$code" = 200 ]; then
    ok=$((ok + 1))
  else
    bad=$((bad + 1)); last="$(date +%T) $code"; [ -n "$first" ] || first=$last
  fi
  sleep 0.2
done
[ -z "$first" ] || echo "first miss $first, last miss $last"
echo "$ok answered 200, $bad did not"
