vim.cmd [[set rtp+=..]]
vim.g.agent_chat = {
	agents = {
		test = {
			cmd = { "npx", "tsx", "agent.ts" }
		}
	}
}

