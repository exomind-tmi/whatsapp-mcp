# WhatsApp for Claude

> **Pre-release.** Work in progress, expect breaking changes.

A plugin that lets Claude read, search and send WhatsApp messages across several linked
accounts. It runs locally: one background process holds WhatsApp connections, and every
Claude session on that device talks to it. The background process talks only to WhatsApp.

The plugin works in all Cowork and Claude Code sessions, but not in the Chat mode of
Claude Desktop: Chat does not load MCP servers from plugins.

## Install

**Claude Desktop (Cowork) and Claude Code**

Customize → Plugins → Add → *Add from a repository* → `exomind-tmi/claude-plugins`,
then install **whatsapp**. Claude Code that is signed in with the same account will
automatically pick it up on every device.

**Claude Code only**

```
claude plugin marketplace add exomind-tmi/claude-plugins
claude plugin install whatsapp@exomind
```

## Where it keeps your data

`~/.mcp/exomind-tmi/whatsapp-mcp/` (override with `WHATSAPP_MCP_HOME`): device keys,
the message archive and logs. Removing the plugin does not delete it.
