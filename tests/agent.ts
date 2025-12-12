#!/usr/bin/env node

import * as acp from "@agentclientprotocol/sdk";
import { Readable, Writable } from "node:stream";
import * as path from "node:path";

interface AgentSession {
	pendingPrompt: AbortController | null;
}

class TestAgent implements acp.Agent {
	private connection: acp.AgentSideConnection;
	private sessions: Map<string, AgentSession>;

	constructor(connection: acp.AgentSideConnection) {
		this.connection = connection;
		this.sessions = new Map();
	}

	async initialize(
		_params: acp.InitializeRequest,
	): Promise<acp.InitializeResponse> {
		return {
			protocolVersion: acp.PROTOCOL_VERSION,
			agentCapabilities: {
				loadSession: false,
			},
		};
	}

	async newSession(
		_params: acp.NewSessionRequest,
	): Promise<acp.NewSessionResponse> {
		const sessionId = Array.from(crypto.getRandomValues(new Uint8Array(16)))
			.map((b) => b.toString(16).padStart(2, "0"))
			.join("");

		this.sessions.set(sessionId, {
			pendingPrompt: null,
		});

		return {
			sessionId,
		};
	}

	async authenticate(
		_params: acp.AuthenticateRequest,
	): Promise<acp.AuthenticateResponse | void> {
		return {};
	}

	async setSessionMode(
		_params: acp.SetSessionModeRequest,
	): Promise<acp.SetSessionModeResponse> {
		return {};
	}

	async prompt(params: acp.PromptRequest): Promise<acp.PromptResponse> {
		const session = this.sessions.get(params.sessionId);

		if (!session) {
			throw new Error(`Session ${params.sessionId} not found`);
		}

		session.pendingPrompt?.abort();
		session.pendingPrompt = new AbortController();

		try {
			await this.handlePrompt(
				params.sessionId,
				params.prompt,
				session.pendingPrompt.signal,
			);
		} catch (err) {
			if (session.pendingPrompt.signal.aborted) {
				return { stopReason: "cancelled" };
			}

			throw err;
		}

		session.pendingPrompt = null;

		return {
			stopReason: "end_turn",
		};
	}

	private async handlePrompt(
		sessionId: string,
		prompt: acp.ContentBlock[],
		abortSignal: AbortSignal,
	): Promise<void> {
		// Extract text from prompt
		let promptText = "";
		for (const block of prompt) {
			if (block.type === "text") {
				promptText += block.text;
			}
		}

		// Rule-based responses based on prompt
		if (promptText === "test:text") {
			await this.handleTextTest(sessionId, abortSignal);
		} else if (promptText === "test:read") {
			await this.handleReadTest(sessionId, abortSignal);
		} else if (promptText === "test:write") {
			await this.handleWriteTest(sessionId, abortSignal);
		} else {
			// Default response
			await this.connection.sessionUpdate({
				sessionId,
				update: {
					sessionUpdate: "agent_message_chunk",
					content: {
						type: "text",
						text: `I received your message: "${promptText}". Use test:text, test:read, or test:write for specific tests.`,
					},
				},
			});
		}
	}

	private async handleTextTest(
		sessionId: string,
		abortSignal: AbortSignal,
	): Promise<void> {
		await this.connection.sessionUpdate({
			sessionId,
			update: {
				sessionUpdate: "agent_message_chunk",
				content: {
					type: "text",
					text: "This is a simple text response for testing. No file operations needed!",
				},
			},
		});
	}

	private formatFileContent(content: string, filePath: string): string {
		const lines = content.split("\n");
		const totalLines = lines.length;
		
		// Format with line numbers like cat -n
		let formatted = "<file>\n";
		lines.forEach((line, index) => {
			const lineNum = (index + 1).toString().padStart(5, "0");
			formatted += `${lineNum}| ${line}\n`;
		});
		formatted += `\n(End of file - total ${totalLines} lines)\n</file>`;
		
		return formatted;
	}

	private async handleReadTest(
		sessionId: string,
		abortSignal: AbortSignal,
	): Promise<void> {
		const testFilePath = path.join(process.cwd(), "test-read.txt");

		await this.connection.sessionUpdate({
			sessionId,
			update: {
				sessionUpdate: "agent_message_chunk",
				content: {
					type: "text",
					text: "I'll read the test file for you using the file system client.",
				},
			},
		});

		await this.simulateDelay(abortSignal, 300);

		// Send tool call
		await this.connection.sessionUpdate({
			sessionId,
			update: {
				sessionUpdate: "tool_call",
				toolCallId: "read_1",
				title: `Reading ${path.basename(testFilePath)}`,
				kind: "read",
				status: "pending",
				locations: [{ path: testFilePath }],
				rawInput: { path: testFilePath },
			},
		});

		// Request permission
		const readPermission = await this.connection.requestPermission({
			sessionId,
			toolCall: {
				toolCallId: "read_1",
				title: `Reading ${path.basename(testFilePath)}`,
				kind: "read",
				status: "pending",
				locations: [{ path: testFilePath }],
				rawInput: { path: testFilePath },
			},
			options: [
				{
					kind: "allow_once",
					name: "Allow reading",
					optionId: "allow",
				},
				{
					kind: "reject_once",
					name: "Reject",
					optionId: "reject",
				},
			],
		});

		if (
			readPermission.outcome.outcome === "selected" &&
			readPermission.outcome.optionId === "allow"
		) {
			try {
				// Use ACP client's ReadTextFile capability
				const result = await this.connection.readTextFile({
					sessionId: sessionId,
					path: testFilePath,
				});

				// Format file content with line numbers
				const formattedContent = this.formatFileContent(result.content, testFilePath);

				await this.connection.sessionUpdate({
					sessionId,
					update: {
						sessionUpdate: "tool_call_update",
						toolCallId: "read_1",
						status: "completed",
						content: [
							{
								type: "content",
								content: {
									type: "text",
									text: formattedContent,
								},
							},
						],
						rawOutput: { content: result.content },
					},
				});

				await this.simulateDelay(abortSignal, 200);

				await this.connection.sessionUpdate({
					sessionId,
					update: {
						sessionUpdate: "agent_message_chunk",
						content: {
							type: "text",
							text: " Successfully read the file!",
						},
					},
				});
			} catch (err) {
				await this.connection.sessionUpdate({
					sessionId,
					update: {
						sessionUpdate: "tool_call_update",
						toolCallId: "read_1",
						status: "failed",
						rawOutput: { error: String(err) },
					},
				});
			}
		} else {
			await this.connection.sessionUpdate({
				sessionId,
				update: {
					sessionUpdate: "agent_message_chunk",
					content: {
						type: "text",
						text: " Read operation was cancelled.",
					},
				},
			});
		}
	}

	private async handleWriteTest(
		sessionId: string,
		abortSignal: AbortSignal,
	): Promise<void> {
		const writeFilePath = path.join(process.cwd(), "test-write.txt");
		const writeContent = `Test data written at ${new Date().toISOString()}`;

		await this.connection.sessionUpdate({
			sessionId,
			update: {
				sessionUpdate: "agent_message_chunk",
				content: {
					type: "text",
					text: "I'll write some test data to a file using the file system client.",
				},
			},
		});

		await this.simulateDelay(abortSignal, 300);

		// Send tool call
		await this.connection.sessionUpdate({
			sessionId,
			update: {
				sessionUpdate: "tool_call",
				toolCallId: "write_1",
				title: `Writing ${path.basename(writeFilePath)}`,
				kind: "edit",
				status: "pending",
				locations: [{ path: writeFilePath }],
				rawInput: { path: writeFilePath, content: writeContent },
			},
		});

		// Request permission
		const writePermission = await this.connection.requestPermission({
			sessionId,
			toolCall: {
				toolCallId: "write_1",
				title: `Writing ${path.basename(writeFilePath)}`,
				kind: "edit",
				status: "pending",
				locations: [{ path: writeFilePath }],
				rawInput: { path: writeFilePath, content: writeContent },
			},
			options: [
				{
					kind: "allow_once",
					name: "Allow writing",
					optionId: "allow",
				},
				{
					kind: "reject_once",
					name: "Reject",
					optionId: "reject",
				},
			],
		});

		if (
			writePermission.outcome.outcome === "selected" &&
			writePermission.outcome.optionId === "allow"
		) {
			try {
				// Use ACP client's WriteTextFile capability
				await this.connection.writeTextFile({
					sessionId: sessionId,
					path: writeFilePath,
					content: writeContent,
				});

				await this.connection.sessionUpdate({
					sessionId,
					update: {
						sessionUpdate: "tool_call_update",
						toolCallId: "write_1",
						status: "completed",
						rawOutput: { success: true, bytesWritten: writeContent.length },
					},
				});

				await this.simulateDelay(abortSignal, 200);

				await this.connection.sessionUpdate({
					sessionId,
					update: {
						sessionUpdate: "agent_message_chunk",
						content: {
							type: "text",
							text: ` Successfully wrote ${writeContent.length} bytes!`,
						},
					},
				});
			} catch (err) {
				await this.connection.sessionUpdate({
					sessionId,
					update: {
						sessionUpdate: "tool_call_update",
						toolCallId: "write_1",
						status: "failed",
						rawOutput: { error: String(err) },
					},
				});
			}
		} else {
			await this.connection.sessionUpdate({
				sessionId,
				update: {
					sessionUpdate: "agent_message_chunk",
					content: {
						type: "text",
						text: " Write operation was cancelled.",
					},
				},
			});
		}
	}

	private simulateDelay(
		abortSignal: AbortSignal,
		ms: number,
	): Promise<void> {
		return new Promise((resolve, reject) =>
			setTimeout(() => {
				if (abortSignal.aborted) {
					reject(new Error("Aborted"));
				} else {
					resolve();
				}
			}, ms),
		);
	}

	async cancel(params: acp.CancelNotification): Promise<void> {
		this.sessions.get(params.sessionId)?.pendingPrompt?.abort();
	}
}

const input = Writable.toWeb(process.stdout);
const output = Readable.toWeb(process.stdin) as ReadableStream<Uint8Array>;

const stream = acp.ndJsonStream(input, output);
new acp.AgentSideConnection((conn) => new TestAgent(conn), stream);
