import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import net from 'node:net';
import test from 'node:test';

test('disposable native MCP fixture exposes distinct A and B inventories', async () => {
  const reservation = net.createServer();
  reservation.listen(0, '127.0.0.1');
  await once(reservation, 'listening');
  const port = reservation.address().port;
  await new Promise((resolve) => reservation.close(resolve));
  const child = spawn(process.execPath, [new URL('./native_mcp_server.mjs', import.meta.url).pathname], {
    env: { ...process.env, MCP_FIXTURE_PORT: String(port), MCP_FIXTURE_TOKEN: 'fixture-token' }, stdio: 'ignore',
  });
  try {
    const request = async (path, method, params = {}) => {
      const response = await fetch(`http://127.0.0.1:${port}${path}`, {
        method: 'POST', headers: { Authorization: 'Bearer fixture-token', 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
        body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
      });
      assert.equal(response.status, 200);
      return (await response.json()).result;
    };
    for (let retry = 0; retry < 50; retry += 1) {
      try { await request('/mcp/a', 'initialize'); break; }
      catch (error) { if (retry === 49) throw error; await new Promise((resolve) => setTimeout(resolve, 20)); }
    }
    const a = await request('/mcp/a', 'tools/list');
    const b = await request('/mcp/b', 'tools/list');
    assert.deepEqual(a.tools.map((tool) => tool.name), ['fixture_a']);
    assert.deepEqual(b.tools.map((tool) => tool.name), ['fixture_b']);
    assert.equal((await request('/mcp/b', 'tools/call', { name: 'fixture_b', arguments: {} })).content[0].text, 'HL319_MCP_B');
    const denied = await fetch(`http://127.0.0.1:${port}/mcp/a`, { method: 'POST' });
    assert.equal(denied.status, 401);
  } finally {
    child.kill();
    await once(child, 'exit');
  }
});
