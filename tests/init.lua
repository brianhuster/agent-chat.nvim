vim.cmd [[set rtp+=..]]
vim.o.clipboard = "unnamedplus"
vim.g.acp = {
	agents = {
		test = {
			cmd = { "npx", "tsx", "agent.ts" },
			mcp = true
		}
	},    mcp = {
        nvim = {
			cmd = { 'nvim-mcp' },
            env = {
				NVIM = vim.v.servername
			}
		}
	}

}

