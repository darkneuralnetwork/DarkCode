# Policy

A policy is one file that says what an install is allowed to do. It covers the
three things worth constraining: which tools may run, how much passes without a
human looking, and which models a repository's code may be sent to.

It is read from `.darkcode/policy.json` in the working directory, then from
`~/.darkcode/policy.json`. The first one found wins. Having none is normal.

## It can only restrict

A policy never grants anything. It can forbid a tool the config allows, shorten
a timeout, or take a model away — it cannot loosen a setting, re-enable a
blocked tool, or extend a limit.

That rule is what makes the file safe to place somewhere the person using the
tool cannot edit. Without it, dropping a policy next to the binary would be a
way to *gain* permissions rather than lose them.

## Example

```json
{
  "tools": {
    "deny": ["terminal:*curl*|sh*"],
    "allow_only": ["read_file", "write_file", "graph_*", "lsp"]
  },
  "permissions": {
    "min_safety_level": "strict",
    "max_approval_timeout_seconds": 120,
    "max_blast_radius": 0.1
  },
  "models": {
    "require_local": true
  }
}
```

## tools

| Field | Effect |
| :-- | :-- |
| `deny` | Added to the config's own deny rules. Same `tool` or `tool:pattern` form. |
| `allow_only` | An allow-list. Anything unlisted is refused. A trailing `*` is a prefix match. |

Both are checked ahead of every permissive path — before the relaxed level,
before a session-wide "allow for session", before any approver is consulted. An
allow-list that a relaxed level could step around would not be one.

They are independent: either refusing is enough. An allow-list does not
re-permit something a deny rule blocked.

## permissions

| Field | Effect |
| :-- | :-- |
| `min_safety_level` | The least strict level permitted (`off`, `normal`, `strict`). |
| `max_approval_timeout_seconds` | Ceiling on how long an approval may block. |
| `max_blast_radius` | Ceiling on the escalation threshold, so central-file edits escalate sooner. |

## models

This is the half that decides whether source leaves the machine.

| Field | Effect |
| :-- | :-- |
| `require_local` | Only self-hosted providers. Enforced on the provider's own local flag, so a hosted endpoint cannot pass by naming itself after a local one. |
| `allow` | An allow-list, matched against the model name and against `provider/model`. |
| `deny` | Checked first; wins over `allow`. |
| `max_input_price`, `max_output_price` | Ceilings in dollars per million tokens. |

A forbidden model is never registered with the router, so no path can reach it
— not the tier lookup, not consensus fan-out, not a role selector. The refusal
is logged, because "no model available for tier reasoning" is a confusing thing
to read when the cause is a policy file.

A model with no catalogue entry is allowed under a price ceiling: refusing what
cannot be priced would block every custom endpoint. `require_local` is the
exception and refuses what it cannot confirm, since "unknown" is not an
acceptable answer to "does this leave the machine".

Provider prefixes are validated when the file loads. Google's models are keyed
under `google`, so `"gemini/*"` names a provider that does not exist — on an
allow-list that would match nothing and silently permit everything, so it is
rejected outright rather than left to fail quietly.

## Failure

A malformed policy is fatal. The difference between no policy and a policy
nobody could parse is the difference between an install that was never
restricted and one that believes it is.
