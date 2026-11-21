-- limen integration for WezTerm
--
-- Renders the context of each pane's directory:
--   - coloured label at the front of the tab title
--   - actor / model / gh user / gcp project in the right status
--   - a marker when a plaintext key still sits in the config
--   - a marker when the project routes through a local gateway
--
-- and starts every new pane through `limen account exec`, so the shell (and
-- every Claude Code session started from it) carries CLAUDE_CONFIG_DIR and
-- CLAUDE_CODE_OAUTH_TOKEN of the account in effect for the pane's directory.
-- The token is resolved by limen at spawn time; it never passes through this
-- config, WezTerm's state or a file. limen.account_env = false switches it off.
--
-- `limen json` costs about 5 ms, so the cache here exists only to avoid
-- redundant work when WezTerm repaints several times in a row — one second,
-- not longer.
--
-- Install: put this file (or a symlink) next to your wezterm.lua, then
--
--   local limen = require 'wezterm-limen'
--   limen.apply(config)
--
-- WezTerm spawns child processes without your shell's environment, so `limen`
-- may not be on its PATH. Point at it explicitly in that case:
--
--   local limen = require 'wezterm-limen'
--   limen.bin = '/Users/you/.local/bin/limen'
--   limen.apply(config)
--
-- LIMEN_BIN is honoured too, for launching WezTerm from a shell that has it.

local wezterm = require 'wezterm'

local M = {}

-- Settable from wezterm.lua; falls back to the environment, then to PATH.
M.bin = os.getenv('LIMEN_BIN') or 'limen'

-- Wrap new panes in `limen account exec` (see apply()).
M.account_env = true

-- Cache: cwd -> { ts = unix_time, ctx = table_or_nil }
local cache = {}
local CACHE_TTL_SEC = 1

-- Colours keyed by label. Extend freely; anything unknown gets the default.
M.palette = {
  levara     = '#7cb342', -- green
  tessera    = '#26a69a', -- teal
  circlead   = '#ffa726', -- orange
  nuncio     = '#42a5f5', -- blue
  limen      = '#8d6e63', -- brown
  atrium     = '#ab47bc', -- violet
  isb        = '#ec407a', -- pink
  turbogruen = '#66bb6a', -- green
  cxo        = '#5c6bc0', -- indigo
  gcp        = '#ef6c50', -- terracotta
}
M.default_color = '#9575cd'
M.dim_color = '#6c7086'

--- Colour for a context, by its label.
---
--- Exact match first, then the longest palette key the label starts with. The
--- prefix step is not a convenience: `label` defaults to the directory name —
--- so `circlead-platform` and `levara-website` would match nothing and every
--- project would come out the same default purple.
--- Longest-first so `limen` cannot win over a hypothetical `limen-extra` entry.
function M.color_for(ctx)
  local label = (ctx and ctx.label or ''):lower()
  if label == '' then return M.default_color end
  if M.palette[label] then return M.palette[label] end

  local best, best_len = nil, 0
  for key, colour in pairs(M.palette) do
    if #key > best_len and label:sub(1, #key) == key then
      best, best_len = colour, #key
    end
  end
  return best or M.default_color
end

--- The label shown in the tab, falling back through label -> actor -> 'limen'.
function M.tab_label(ctx)
  if not ctx then return nil end
  if ctx.label and ctx.label ~= '' then return ctx.label end
  if ctx.actor and ctx.actor ~= '' then return ctx.actor end
  return 'limen'
end

--- The right-status segments, in display order.
-- Pure and exported so the self-test can assert on it without a terminal.
function M.status_parts(ctx)
  local parts = {}
  if not ctx then return parts end
  if ctx.actor and ctx.actor ~= '' then
    table.insert(parts, ctx.actor)
  end
  if ctx.model and ctx.model ~= '' then
    table.insert(parts, ctx.model)
  end
  if ctx.github_user and ctx.github_user ~= '' then
    table.insert(parts, 'gh:' .. ctx.github_user)
  end
  if ctx.gcloud_project and ctx.gcloud_project ~= '' then
    table.insert(parts, 'gcp:' .. ctx.gcloud_project)
  end
  -- A local gateway means the model route belongs to the project rather than to
  -- the shell it was started from; worth seeing at a glance.
  if ctx.gateway and ctx.gateway ~= '' then
    table.insert(parts, 'gw')
  end
  -- limen json distinguishes a resolvable key (env or keychain) from a plaintext
  -- key still in the config file. Only the latter is a warning.
  if ctx.api_key_in_config then
    table.insert(parts, '!key-in-config')
  end
  return parts
end

-- WezTerm's objects raise on an unknown field instead of returning nil.
local function field(obj, key)
  local ok, v = pcall(function() return obj[key] end)
  if ok then return v end
  return nil
end

--- The pane's working directory as a path, or nil.
--- Accepts both shapes WezTerm hands out: a Pane object (status events) with a
--- get_current_working_dir() method, and the PaneInformation table that
--- format-tab-title passes as tab.active_pane, which has a
--- current_working_dir field instead. Exported for the self-test.
function M.cwd_from_pane(pane)
  if not pane then return nil end
  local uri
  local getter = field(pane, 'get_current_working_dir')
  if type(getter) == 'function' then
    local ok, v = pcall(getter, pane)
    if ok then uri = v end
  else
    uri = field(pane, 'current_working_dir')
  end
  if not uri then return nil end
  if type(uri) ~= 'string' then
    local path = field(uri, 'file_path')
    if path and path ~= '' then return path end
    return nil
  end
  if type(uri) == 'string' then
    return uri:match('^file://[^/]*(/.*)$')
  end
  return nil
end

--- Reads `limen json` for a directory. Returns nil when there is no context,
--- which is a normal state and not an error.
--- cache_only = true never spawns a process and returns whatever the cache
--- holds, stale or not: format-tab-title runs where WezTerm forbids child
--- processes, and the status events keep the cache fresh for it.
function M.read_context(cwd, cache_only)
  if not cwd then return nil end
  local now = os.time()
  local hit = cache[cwd]
  if cache_only then
    return hit and hit.ctx or nil
  end
  if hit and (now - hit.ts) < CACHE_TTL_SEC then
    return hit.ctx
  end

  local success, stdout = wezterm.run_child_process({
    'bash', '-c',
    'cd ' .. wezterm.shell_quote_arg(cwd) .. ' && ' .. M.bin .. ' json 2>/dev/null'
  })
  if not success or not stdout or stdout == '' then
    cache[cwd] = { ts = now, ctx = nil }
    return nil
  end

  local ok, parsed = pcall(wezterm.json_parse, stdout)
  -- Without a context limen prints `{}` and exits 0, so an empty table is the
  -- expected answer rather than a failure. `root` is what distinguishes them.
  if not ok or type(parsed) ~= 'table' or not parsed.root or parsed.root == '' then
    cache[cwd] = { ts = now, ctx = nil }
    return nil
  end
  cache[cwd] = { ts = now, ctx = parsed }
  return parsed
end

--- Optional hook, set from wezterm.lua: function(tab, ctx) -> text, color.
--- Its text is shown as a segment right after the label (ctx may be nil, then
--- after the tab number). Use it for things only wezterm.lua knows, such as
--- which Claude account a pane was started with. nil or '' shows nothing.
M.tab_suffix = nil

local function suffix_items(tab, ctx)
  if not M.tab_suffix then return {} end
  local ok, text, color = pcall(M.tab_suffix, tab, ctx)
  if not ok or not text or text == '' then return {} end
  return {
    'ResetAttributes',
    { Foreground = { Color = color or M.dim_color } },
    { Text = ' ' .. text },
    'ResetAttributes',
  }
end

--- The tab title as wezterm.format items: coloured label (or tab number),
--- the optional M.tab_suffix segment, then the title.
--- Pure and exported so the self-test can assert on it without a terminal.
function M.tab_title_items(tab, ctx, title)
  local items
  if not ctx then
    items = { { Text = ' ' .. tab.tab_index + 1 .. ':' } }
  else
    items = {
      { Background = { Color = M.color_for(ctx) } },
      { Foreground = { Color = '#1a1a1a' } },
      { Text = ' ' .. M.tab_label(ctx) .. ' ' },
    }
  end
  for _, it in ipairs(suffix_items(tab, ctx)) do table.insert(items, it) end
  table.insert(items, 'ResetAttributes')
  table.insert(items, { Text = ' ' .. title .. ' ' })
  return items
end

wezterm.on('format-tab-title', function(tab, tabs, panes, config, hover, max_width)
  local pane = tab.active_pane
  local ctx = M.read_context(M.cwd_from_pane(pane), true)
  local title = tab.tab_title
  if title == nil or title == '' then title = pane.title end
  return M.tab_title_items(tab, ctx, title)
end)

-- Fill the cache for every tab of the window, so format-tab-title (which may
-- only read the cache) has a context for inactive tabs too.
local function refresh_tabs(window)
  local ok, tabs = pcall(function() return window:mux_window():tabs() end)
  if not ok or not tabs then return end
  for _, t in ipairs(tabs) do
    local pok, p = pcall(function() return t:active_pane() end)
    if pok and p then M.read_context(M.cwd_from_pane(p)) end
  end
end

wezterm.on('update-right-status', function(window, pane)
  refresh_tabs(window)
  local cwd = M.cwd_from_pane(pane)
  local ctx = M.read_context(cwd)

  if not ctx then
    -- A known directory without a context gets a dim hint, so "no context here"
    -- stays distinguishable from "no cwd yet".
    if cwd then
      window:set_right_status(wezterm.format({
        { Foreground = { Color = M.dim_color } },
        { Text = '  no limen — limen init  ' },
      }))
    else
      window:set_right_status('')
    end
    return
  end

  window:set_right_status(wezterm.format({
    { Foreground = { Color = M.color_for(ctx) } },
    { Text = '  ' .. table.concat(M.status_parts(ctx), ' · ') .. '  ' },
  }))
end)

--- argv that runs `args` in the account environment of its directory:
--- limen resolves the token and execs the command, so the caller never
--- handles it. Use it for your own SpawnCommand args too.
function M.with_account(args)
  local argv = { M.bin, 'account', 'exec', '--' }
  for _, a in ipairs(args) do table.insert(argv, a) end
  return argv
end

function M.apply(config)
  config.use_fancy_tab_bar = false
  config.tab_max_width = 48
  if M.account_env then
    local prog = config.default_prog or { os.getenv('SHELL') or '/bin/zsh', '-l' }
    -- Already wrapped (apply called twice): leave it.
    if prog[2] ~= 'account' then
      config.default_prog = M.with_account(prog)
    end
  end
end

return M
