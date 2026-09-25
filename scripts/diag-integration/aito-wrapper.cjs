// Control only the approved fixture. No business code is copied here.
const readline = require('node:readline');
const path = require('node:path');
const fs = require('node:fs');
async function main() {
  const fixture = await require(path.join(process.argv[2], 'scripts/diagnostics/isolatedServer.js')).start({quiet: true});
  const LoggingService = require(path.join(process.argv[2], 'src/utils/LoggingService'));
  const reply = value => process.stdout.write(JSON.stringify(value) + '\n');
  reply({fixture: 'aito', address: fixture.address, root: fixture.root, pid: process.pid});
  const input = readline.createInterface({input: process.stdin});
  for await (const line of input) {
    const command = JSON.parse(line);
    if (command.op === 'debug') LoggingService.setLevel(command.enabled ? 'DEBUG' : 'INFO');
    else if (command.op === 'reconnect') await fixture.reconnect();
    else if (command.op === 'snapshot') {
      reply({ack: 'snapshot', dispatches: fixture.dispatches.length, attempts: fixture.registry.generationAttempts.size});
      continue;
    } else if (command.op === 'close') {
      await fixture.close();
      reply({closed: true, dispatches: fixture.dispatches.length, bytes: fs.statSync(path.join(fixture.root, 'diagnostics.jsonl')).size});
      input.close();
      return;
    } else throw new Error('unknown fixture command');
    reply({ack: command.op});
  }
  await fixture.close();
}
main().catch(() => { process.stderr.write('fixture control failed\n'); process.exitCode = 1; });
