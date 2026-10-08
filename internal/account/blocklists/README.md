# Username blocklists

Names in these lists cannot be registered (web signup or `gitserver user add`).
They are embedded into the binary; edit and rebuild to change them.

| File | Source | License |
|---|---|---|
| `shouldbee-reserved-usernames.txt` | https://github.com/shouldbee/reserved-usernames | MIT, see `shouldbee-reserved-usernames.LICENSE` |
| `big-username-blocklist.txt` | https://github.com/marteinn/The-Big-Username-Blocklist | MIT, see `big-username-blocklist.LICENSE` |

The short list of staff and system names in `../usernames.go` (`blockedUserNames`) is checked as well.
Any other `*.txt` file added here is picked up too: one name per line, `#` starts a comment.
