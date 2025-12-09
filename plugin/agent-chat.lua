local command = vim.api.nvim_create_user_command

command("AgentChatStart", function(opts)
	require("agent-chat").start(opts.args)
end, {
	nargs = 1,
    desc = "Start ACP connection and open chat window.",
	complete = "custom,v:lua.require'agent-chat'.acpstart_complete"
})

command("AgentChatStop", function()
	require("agent-chat").stop(0)
end, { desc = "Stop ACP connection for current buffer" })
