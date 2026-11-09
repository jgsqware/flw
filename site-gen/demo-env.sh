# source me: run flw against the fake glab, with no real config, cache or Rex
D="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export PATH="$D/bin:$PATH"
export FLW_CONFIG="$D/demo-home/config/config.yaml"
export XDG_STATE_HOME="$D/demo-home/state"
export FLW_NO_CACHE=1
export FLW_CODE_ROOTS="$D/demo-home/none"
[ -n "$FLW_DEMO_KEEP_REX" ] || unset REX_SESSION REX_BLOCK
export COLORTERM=truecolor
export FLW_DEMO_NOW="${FLW_DEMO_NOW:-$(date -u +%s)}"
