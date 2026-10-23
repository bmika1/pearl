import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {readFileSync} from 'node:fs';
import {createRequire} from 'node:module';
import {test} from 'node:test';
import {fileURLToPath} from 'node:url';
import ts from 'typescript';

const sourcePath = fileURLToPath(new URL('../src/main/services/process-shutdown.ts', import.meta.url));
const {outputText} = ts.transpileModule(readFileSync(sourcePath, 'utf8'), {
  compilerOptions: {module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022},
});
const module = {exports: {}};
new Function('require', 'module', 'exports', outputText)(createRequire(sourcePath), module, module.exports);
const {shutdownProcess} = module.exports;

class FakeProcess extends EventEmitter {
  constructor(exitsOn = []) {
    super();
    this.pid = 4242;
    this.exitCode = null;
    this.signalCode = null;
    this.exitsOn = exitsOn;
    this.signals = [];
  }

  kill(signal) {
    this.signals.push(signal);
    if (this.exitsOn.includes(signal)) this.exit(null, signal);
    return true;
  }

  exit(code, signal = null) {
    this.exitCode = code;
    this.signalCode = signal;
    this.emit('exit', code, signal);
  }
}

function shutdown(proc, options = {}) {
  return shutdownProcess(proc, {
    requestStop: async () => {},
    stopTimeoutMs: 50,
    signalTimeoutMs: 50,
    platform: 'linux',
    ...options,
  });
}

test('a process that exits when asked to stop is not signalled', async () => {
  const proc = new FakeProcess();
  const requestStop = async () => {
    setTimeout(() => proc.exit(0), 5);
  };

  assert.equal(await shutdown(proc, {requestStop, stopTimeoutMs: 10_000}), true);
  assert.deepEqual(proc.signals, []);
});

test('a process that ignores the stop request is terminated, then killed', async () => {
  for (const [exitsOn, signals] of [
    [['SIGTERM'], ['SIGTERM']],
    [['SIGKILL'], ['SIGTERM', 'SIGKILL']],
    [[], ['SIGTERM', 'SIGKILL']],
  ]) {
    const proc = new FakeProcess(exitsOn);
    assert.equal(await shutdown(proc), exitsOn.length > 0);
    assert.deepEqual(proc.signals, signals);
  }
});

test('a stop request that fails is not waited for', async () => {
  const proc = new FakeProcess(['SIGTERM']);
  const requestStop = async () => {
    throw new Error('connect ECONNREFUSED');
  };

  const started = Date.now();
  assert.equal(await shutdown(proc, {requestStop, stopTimeoutMs: 10_000}), true);
  assert.ok(Date.now() - started < 5000);
  assert.deepEqual(proc.signals, ['SIGTERM']);
});

test('skipping the termination request goes straight to the kill', async () => {
  const proc = new FakeProcess(['SIGKILL']);

  assert.equal(await shutdown(proc, {skipTerminate: true}), true);
  assert.deepEqual(proc.signals, ['SIGKILL']);
});

test('on Windows the process is asked to close instead of being sent SIGTERM', async () => {
  const closing = new FakeProcess();
  const closeWindowsProcess = async () => {
    setTimeout(() => closing.exit(0), 5);
    return true;
  };
  assert.equal(await shutdown(closing, {platform: 'win32', closeWindowsProcess}), true);
  assert.deepEqual(closing.signals, []);

  // A console program can't be asked to close, so it is killed without waiting.
  const consoleProgram = new FakeProcess(['SIGKILL']);
  const started = Date.now();
  const exited = await shutdown(consoleProgram, {
    platform: 'win32',
    signalTimeoutMs: 10_000,
    closeWindowsProcess: async () => false,
  });
  assert.equal(exited, true);
  assert.ok(Date.now() - started < 5000);
  assert.deepEqual(consoleProgram.signals, ['SIGKILL']);
});
