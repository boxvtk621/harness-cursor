#!/usr/bin/env node
// Disposable Streamable HTTP MCP fixture for the HL-319 A -> B acceptance.
// Bind this container only to an isolated test network; no provider keys pass
// through the fixture. Each path advertises exactly one distinct tool.
import http from 'node:http';

const port = Number(process.env.MCP_FIXTURE_PORT || 8765);
const token = process.env.MCP_FIXTURE_TOKEN || '';
if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('invalid fixture port');

const server = http.createServer(async (request, response) => {
  response.setHeader('Cache-Control', 'no-store');
  const variant = request.url === '/mcp/a' ? 'a' : request.url === '/mcp/b' ? 'b' : null;
  if (!variant) { response.writeHead(404).end(); return; }
  if (token && request.headers.authorization !== `Bearer ${token}`) { response.writeHead(401).end(); return; }
  if (request.method === 'GET') { response.writeHead(405, { Allow: 'POST' }).end(); return; }
  if (request.method !== 'POST') { response.writeHead(405).end(); return; }
  const chunks = [];
  for await (const chunk of request) {
    chunks.push(chunk);
    if (chunks.reduce((sum, item) => sum + item.length, 0) > 65536) { response.writeHead(413).end(); return; }
  }
  let message;
  try { message = JSON.parse(Buffer.concat(chunks).toString('utf8')); }
  catch { response.writeHead(400).end(); return; }
  if (!message || message.jsonrpc !== '2.0' || typeof message.method !== 'string') { response.writeHead(400).end(); return; }
  if (message.id === undefined) { response.writeHead(202).end(); return; }
  let result;
  const tool = `fixture_${variant}`;
  switch (message.method) {
    case 'initialize':
      result = { protocolVersion: '2025-03-26', capabilities: { tools: {} }, serverInfo: { name: `hl319-${variant}`, version: '1.0.0' } };
      break;
    case 'tools/list':
      result = { tools: [{ name: tool, description: `Return a stable ${variant.toUpperCase()} marker for HL-319`, inputSchema: { type: 'object', properties: {}, additionalProperties: false } }] };
      break;
    case 'tools/call':
      if (message.params?.name !== tool) { response.writeHead(404).end(); return; }
      result = { content: [{ type: 'text', text: `HL319_MCP_${variant.toUpperCase()}` }] };
      break;
    default:
      response.writeHead(404).end(); return;
  }
  response.writeHead(200, { 'Content-Type': 'application/json' });
  response.end(JSON.stringify({ jsonrpc: '2.0', id: message.id, result }));
});

server.listen(port, '0.0.0.0');
