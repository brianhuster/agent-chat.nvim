local M = {}

---@class AcpAgentConfig
---@field cmd string[] Command to start the agent (e.g., {"opencode", "acp"})
---@field env table<string, string>? Optional environment variables

---@class AcpConfig
---@field agents? table<string, AcpAgentConfig> Mapping of agent names to their configurations

---@class acpSessionModes
---@field CurrentModeId string
---@field AvailableModes { Description: string, Id: string, Name: string }[]

-- Get the directory where this script is located
local script_path = debug.getinfo(1, "S").source:sub(2)
local plugin_dir = vim.fs.dirname(vim.fs.dirname(script_path))

---@class AcpState
---@field rpc_host_job_id number? Job ID of the RPC host process
---@field sessions table<number, { agent: string, window: number?, modes: acpSessionModes? }> Active sessions per buffer
M.state = {
	rpc_host_job_id = nil, -- Single RPC host for all sessions
	sessions = {},      -- { [bufnr] = { agent = "opencode", window = win_id } }
}

---@type AcpConfig
local default_config = {}

---@type AcpConfig
M.config = vim.tbl_deep_extend("force", default_config, vim.g.agent_chat or {})

-- Start RPC host if not already running
local function ensure_rpc_host()
	if M.state.rpc_host_job_id then
		return M.state.rpc_host_job_id
	end

	-- Start the RPC host
	M.state.rpc_host_job_id = vim.fn.jobstart({ vim.fs.joinpath(plugin_dir, "bin", "acp-nvim") }, {
		rpc = true,
		on_exit = function(_, exit_code)
			M.state.rpc_host_job_id = nil
			M.state.sessions = {}
			if exit_code ~= 0 then
				vim.notify("ACP RPC host exited with code " .. exit_code, vim.log.levels.ERROR)
			end
		end,
		on_stderr = function(_, data)
			if data then
				for _, line in ipairs(data) do
					if line ~= "" then
						vim.notify("ACP: " .. line, vim.log.levels.ERROR)
					end
				end
			end
		end,
	})

	if M.state.rpc_host_job_id == 0 then
		vim.notify("Failed to start ACP: invalid arguments", vim.log.levels.ERROR)
		M.state.rpc_host_job_id = nil
		return nil
	elseif M.state.rpc_host_job_id == -1 then
		vim.notify("Failed to start ACP: binary not found", vim.log.levels.ERROR)
		M.state.rpc_host_job_id = nil
		return nil
	end

	return M.state.rpc_host_job_id
end

-- Start the ACP connection for a buffer
---@param agent string
function M.start(agent)
	-- Ensure RPC host is running
	local cmd = M.config.agents[agent].cmd
	local job_id = ensure_rpc_host()
	if not job_id then
		return
	end

	-- Create new buffer
	local bufnr = vim.api.nvim_create_buf(false, true)
	vim.api.nvim_buf_set_name(bufnr, "ACP: " .. agent)

	-- Track the session
	M.state.sessions[bufnr] = { agent = agent, modes = nil }

	-- Initialize ACP connection for this buffer
	local opts = {
		env = M.config.agents[agent].env or vim.empty_dict(),
	}
	vim.rpcnotify(job_id, "ACPStart", bufnr, cmd, opts)
end

-- Stop the ACP connection for a buffer
---@param bufnr number
local function stop(bufnr)
	if not M.state.sessions[bufnr] then
		vim.notify("No ACP session in this buffer", vim.log.levels.WARN)
		return
	end

	if not M.state.rpc_host_job_id then
		M.state.sessions[bufnr] = nil
		return
	end

	-- Stop the session
	local ok, err = pcall(vim.rpcrequest, M.state.rpc_host_job_id, "ACPStop", bufnr)
	if not ok then
		vim.notify("Failed to stop ACP session: " .. vim.inspect(err), vim.log.levels.ERROR)
	end
	M.state.sessions[bufnr] = nil

	-- If no sessions left, optionally stop RPC host
	if vim.tbl_count(M.state.sessions) == 0 then
		vim.fn.jobstop(M.state.rpc_host_job_id)
		M.state.rpc_host_job_id = nil
	end

	vim.notify("ACP session stopped", vim.log.levels.INFO)
end
--- Change ACP mode for a buffer
--- Only called from Go
--- @param bufnr number
--- @param mode_id string
local function set_mode(bufnr, mode_id)
	local ok, result = pcall(vim.rpcrequest, M.state.rpc_host_job_id, "ACPSetMode", bufnr, mode_id)
	if not ok then
		vim.notify("Failed to set ACP mode: " .. vim.inspect(result), vim.log.levels.ERROR)
		return
	end
	M.state.sessions[bufnr].modes.CurrentModeId = result --[[@as string]]
end

-- Show the ACP buffer in a window
---@param bufnr number
local function show(bufnr)
    if bufnr == 0 then
        bufnr = vim.api.nvim_get_current_buf()
    end

    if not vim.api.nvim_buf_is_valid(bufnr) then
        vim.notify("Invalid buffer", vim.log.levels.ERROR)
        return
    end

    local session = M.state.sessions[bufnr]
    if not session then
        vim.notify("No ACP session in this buffer", vim.log.levels.WARN)
        return
    end

    -- Check if already visible in a window
    if session.window and vim.api.nvim_win_is_valid(session.window) then
        vim.api.nvim_set_current_win(session.window)
        return
    end

    -- Create a split window
    vim.cmd("vsplit")
    local window = vim.api.nvim_get_current_win()
    vim.api.nvim_win_set_buf(window, bufnr)
    vim.wo[window].wrap = true
    vim.wo[window].linebreak = true

    session.window = window

    -- Go to the end of the buffer and enter insert mode
    vim.cmd("normal! G")
    vim.cmd("startinsert")
end

-- Send a prompt to agent
---@param bufnr number
---@param text string
local function send_prompt(bufnr, text)
	if not M.state.rpc_host_job_id then
		vim.notify("ACP not running. Run :AcpStart first.", vim.log.levels.ERROR)
		return
	end

	if not M.state.sessions[bufnr] then
		vim.notify("No ACP session in this buffer", vim.log.levels.WARN)
		return
	end

	if not text or text == "" then
		return
	end

	vim.rpcnotify(M.state.rpc_host_job_id, "ACPSendPrompt", bufnr, text)
end

--- Called from Go
---@param bufnr number
---@param opts { modes: acpSessionModes }
function M.set_and_show_prompt_buf(bufnr, opts)
	vim.bo[bufnr].buftype = "prompt"
	vim.bo[bufnr].bufhidden = "hide"
	vim.bo[bufnr].swapfile = false
	vim.bo[bufnr].filetype = "markdown"

	M.state.sessions[bufnr].modes = opts.modes

	vim.fn.prompt_setprompt(bufnr, "> ")
	vim.fn.prompt_setcallback(bufnr, function(text)
		M.append_text(bufnr, "\n\n")
		send_prompt(bufnr, text)
	end)

	vim.fn.prompt_setinterrupt(bufnr, function()
		M.cancel(bufnr)
	end)

	show(bufnr)

	local bufcommand = vim.api.nvim_buf_create_user_command

	bufcommand(bufnr, "AcpSetMode", function(cmd)
		set_mode(bufnr, cmd.args)
	end, {
		nargs = 1,
		desc = "Set ACP mode for this buffer",
		complete = "custom,v:lua.require'acp'.acpsetmode_complete"
	})

	bufcommand(bufnr, "AcpStop", function()
		stop(bufnr)
	end, { desc = "Stop ACP connection for current buffer" })

	-- Auto-cleanup when buffer is deleted
	vim.api.nvim_create_autocmd("BufDelete", {
		buffer = bufnr,
		callback = function(args)
			stop(args.buf)
		end
	})
end

-- Cancel the current operation
---@param bufnr number
function M.cancel(bufnr)
	if not M.state.rpc_host_job_id then
		vim.notify("ACP not running", vim.log.levels.WARN)
		return
	end

	if not M.state.sessions[bufnr] then
		vim.notify("No ACP session in this buffer", vim.log.levels.WARN)
		return
	end

	vim.rpcnotify(M.state.rpc_host_job_id, "ACPCancel", bufnr)
end

-- Append text to a specific buffer
-- Also called from Go
---@param bufnr number
---@param text string
function M.append_text(bufnr, text)
	if not vim.api.nvim_buf_is_valid(bufnr) then
		return
	end

	vim.schedule(function()
		-- Get the prompt line position using the ': mark
		local prompt_pos = vim.api.nvim_buf_get_mark(bufnr, ":")
		local prompt_line = prompt_pos[1] -- 1-indexed line number

		-- Get the line just before the prompt (where we append content)
		local content_line_idx = prompt_line - 2 -- 0-indexed (prompt_line - 1 - 1)

		if content_line_idx < 0 then
			-- No content line exists yet, insert a new line before prompt
			vim.api.nvim_buf_set_lines(bufnr, 0, 0, false, { "" })
			content_line_idx = 0
		end

		-- Get the current content of that line
		local current_line = vim.api.nvim_buf_get_lines(bufnr, content_line_idx, content_line_idx + 1, false)[1] or ""

		-- Append the new text to the current line
		local new_text = current_line .. text

		-- Split by newlines if the text contains them
		local lines = vim.split(new_text, "\n", { plain = true })

		-- Replace the current line and add any additional lines
		vim.api.nvim_buf_set_lines(bufnr, content_line_idx, content_line_idx + 1, false, lines)

		-- Scroll to the bottom if the window is visible
		local session = M.state.sessions[bufnr]
		if session and session.window and vim.api.nvim_win_is_valid(session.window) then
			local new_line_count = vim.api.nvim_buf_line_count(bufnr)
			vim.api.nvim_win_set_cursor(session.window, { new_line_count, 0 })
		end
	end)
end

---@return string
function M.acpstart_complete()
	return vim.iter(vim.tbl_keys(M.config.agents)):join("\n")
end

function M.acpsetmode_complete()
	local buf = vim.api.nvim_get_current_buf()
	return vim.iter(M.state.sessions[buf].modes.AvailableModes):map(function(mode)
		return mode.Id
	end):join("\n")
end

return M
