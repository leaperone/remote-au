---
name: Bug report
about: Report a reproducible problem with remote-au
title: ""
labels: bug
assignees: ""
---

## Summary

What happened, and what did you expect?

## Steps to Reproduce

1.
2.
3.

## Environment

- OS:
- remote-au version (`remote-au version`):
- Install method:
- Audio device(s) from `remote-au devices`:

## Logs

Run with diagnostics enabled when useful:

```sh
remote-au --log-level debug --log-format text <command>
```

For `devices --json`, include stderr separately from stdout.

## Network Notes

- Same LAN/VLAN:
- Firewall rules for TCP/UDP audio port:
- Firewall rules for UDP discovery ports:
