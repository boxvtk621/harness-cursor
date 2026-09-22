#!/usr/bin/env node
import { AuthenticationError, Cursor } from '@cursor/sdk';

try {
  if (!process.env.CURSOR_API_KEY) process.exit(3);
  await Cursor.me();
  process.stdout.write('{"status":"authenticated"}\n');
} catch (error) {
  const status = Number(error?.status ?? error?.cause?.status);
  if (error instanceof AuthenticationError || status === 401 || status === 403) {
    process.stdout.write('{"status":"invalid"}\n');
    process.exit(3);
  }
  process.stdout.write('{"status":"unavailable"}\n');
  process.exit(4);
}
