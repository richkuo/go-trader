RETIRED_VENUE_ISSUE = "issue 2073"
SUPPORTED_OPTION_PLATFORMS = ("ibkr", "robinhood")
RETIRED_VENUE_RECOVERY = (
    "on the previous release close or settle every open position of the affected strategy, verify its "
    "positions and option_positions rows are flat, stop every writer, back up the config and each state "
    "file with its storage mapping, then remove the retired settings (docs/POST_UPDATE_HISTORY.md)")


def _refusal(subject, what):
    return (f"{subject}: {what} is retired; Deribit support and OKX options support were removed in "
            f"{RETIRED_VENUE_ISSUE}; {RETIRED_VENUE_RECOVERY}")


def _venue(value):
    return str(value or "").strip().lower()


def platform_arg_values(args):
    if not isinstance(args, list):
        return []
    out = []
    for i, arg in enumerate(args):
        text = str(arg)
        if text.startswith("--platform="):
            out.append(text.split("=", 1)[1])
        elif text == "--platform" and i + 1 < len(args):
            out.append(str(args[i + 1]))
    return out


def strategy_venue_refusals(sc, index=0):
    if not isinstance(sc, dict):
        return []
    sid = sc.get("id") if isinstance(sc.get("id"), str) else ""
    subject = f"strategy[{sid}]" if sid else f"strategy[{index}]"
    platform = sc.get("platform") if isinstance(sc.get("platform"), str) else ""
    venue = _venue(platform)
    lower_id = sid.strip().lower()
    is_options = str(sc.get("type") or "").strip() == "options"
    errs = []
    if venue == "deribit":
        errs.append(_refusal(subject, f"platform {platform!r}"))
    if lower_id.startswith("deribit-"):
        errs.append(_refusal(subject, "the strategy id prefix 'deribit-'"))
    for value in platform_arg_values(sc.get("args")):
        nv = _venue(value)
        if nv == "deribit":
            errs.append(_refusal(subject, f"the script argument --platform={value}"))
        elif nv == "okx" and is_options:
            errs.append(_refusal(subject, f"the options script argument --platform={value}"))
    if not is_options:
        return errs
    if venue == "okx":
        errs.append(_refusal(subject, f"options on platform {platform!r}"))
    elif venue == "":
        if lower_id.startswith("okx-"):
            errs.append(_refusal(subject, "an options strategy with the 'okx-' id prefix and no platform field"))
        errs.append(f"{subject}: an options strategy needs an explicit platform field ('ibkr' or 'robinhood'); "
                    f"the id prefix no longer selects an options venue ({RETIRED_VENUE_ISSUE})")
    return errs


def config_venue_refusals(cfg):
    if not isinstance(cfg, dict):
        return []
    errs = []
    platforms = cfg.get("platforms")
    if isinstance(platforms, dict):
        for key in sorted(platforms):
            if _venue(key) == "deribit":
                errs.append(_refusal("config platforms", f"the settings key {key!r}"))
    strategies = cfg.get("strategies")
    if isinstance(strategies, list):
        for i, sc in enumerate(strategies):
            errs.extend(strategy_venue_refusals(sc, i))
    return errs


def require_admitted_venues(cfg, label="config"):
    errs = config_venue_refusals(cfg)
    if errs:
        raise ValueError(f"{label}: config validation errors:\n  " + "\n  ".join(errs))
