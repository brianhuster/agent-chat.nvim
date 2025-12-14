local command = vim.api.nvim_create_user_command

command("AcpStart", function(opts)
	require("acp").start(opts.args)
end, {
	nargs = 1,
    desc = "Start ACP connection and open chat window.",
	complete = "custom,v:lua.require'acp'.acpstart_complete"
})
