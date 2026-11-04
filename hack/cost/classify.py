import re
VENDORS = ("blacksmith", "buildjet", "depot", "namespace", "warp", "ubicloud", "runs-on", "cirun", "actuated", "nscloud", "arc-", "gha-runner-scale-set")
def labels(ro):
    if isinstance(ro, str): return [ro]
    if isinstance(ro, list): return [str(x) for x in ro]
    if isinstance(ro, dict):
        l = ro.get("labels"); out = ["__group__"] if ro.get("group") else []
        return out + (labels(l) if l is not None else [])
    return []
def cores(s):
    m = re.search(r"(\d+)[-_ ]?(?:v?cpus?|cores?|vcpu|x\b)", s) or re.search(r"[-_](\d+)$", s)
    return int(m.group(1)) if m else None
def classify(ro):
    """A price tier for a runs-on value, or None if it is an expression."""
    if ro == "__reusable__": return "reusable"
    ls = [l.lower().strip() for l in labels(ro)]
    if not ls: return "none"
    j = " ".join(ls)
    if "${{" in j: return None
    if "__group__" in ls: return "larger-or-self-hosted"
    for v in VENDORS:
        if v in j: return f"third-party/{cores(j) or '?'}core"
    if "self-hosted" in ls: return "self-hosted"
    for l in ls:
        if l.startswith("macos"):
            return "macos-large" if ("large" in l) else "macos"
        if l.startswith("windows"):
            if cores(l): return f"windows-{cores(l)}core"
            return "windows-arm" if "arm" in l else "windows"
        if l == "ubuntu-slim": return "linux-1core-slim"
        if l.startswith("ubuntu"):
            c = cores(l.replace("24.04", "").replace("22.04", "").replace("20.04", ""))
            arm = "arm" in l
            if c and c != 2: return f"linux{'-arm' if arm else ''}-{c}core"
            return "linux-arm" if arm else "linux"
    return "custom"   # a GitHub larger runner by name, or self-hosted
