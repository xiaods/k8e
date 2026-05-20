# Triage Labels

The engineering skills use five canonical triage roles. This file maps those roles to the label strings used in this repo's GitHub Issues.

| Skill role | GitHub label | Meaning |
|---|---|---|
| `needs-triage` | `needs-triage` | Maintainer needs to evaluate |
| `needs-info` | `needs-info` | Waiting on reporter for more info |
| `ready-for-agent` | `ready-for-agent` | Fully specified, AFK-ready |
| `ready-for-human` | `ready-for-human` | Needs human implementation |
| `wontfix` | `wontfix` | Will not be actioned |

To add a label via the `gh` CLI:

```bash
gh issue edit <number> --add-label "needs-triage"
```