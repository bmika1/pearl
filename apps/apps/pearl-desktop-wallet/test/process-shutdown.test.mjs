import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {test} from 'node:test';
import {loadSource} from './load-source.mjs';

const {shutdownProcess, terminatePids} = loadSource()('src/main/services/process-shutdown.ts');

class FakeProcess extends EventEmitter {
  constructor({exitsOn = [], events = []} = {}) {
    super();
    this.pid = 4242;
    this.exitCode = null;
    this.signalCode = null;
    this.signals = [];
    this.exitsOn = new Set(exitsOn);
    this.events = events;
  }

  kill(signal) {
    this.events.push(`signal ${signal}`);
    this.signals.push(signal);
    if (this.exitsOn.has(signal)) this.exit(null, signal);
    return true;
  }

  exit(code, signal = null) {
    this.exitCode = code;
    this.signalCode = signal;
    this.emit('exit', code, signal);
  }
}

const timeouts = {stopTimeoutMs: 60, terminateTimeoutMs: 60, killTimeoutMs: 60};

function stopOptions(proc, overrides = {}) {
  return {
    requestStop: async () => {
      proc.events.push('stop request');
    },
    ...timeouts,
    platform: 'linux',
    ...overrides,
  };
}

test('does nothing for a process that already exited', async () => {
  const proc = new FakeProcess();
  proc.exit(0);
  let requested = false;

  const outcome = await shutdownProcess(proc, stopOptions(proc, {requestStop: async () => (requested = true)}));

  assert.equal(outcome, 'already-exited');
  assert.equal(requested, false);
  assert.deepEqual(proc.signals, []);
});

test('a process that exits after the stop request is never signalled', async () => {
  const proc = new FakeProcess();
  const requestStop = async () => {
    setTimeout(() => proc.exit(0), 5);
  };

  const outcome = await shutdownProcess(proc, stopOptions(proc, {requestStop, stopTimeoutMs: 5000}));

  assert.equal(outcome, 'stopped');
  assert.deepEqual(proc.signals, []);
});

test('a process that exits while the stop request is pending counts as stopped', async () => {
  const proc = new FakeProcess();
  const requestStop = async () => {
    proc.exit(0);
    await new Promise(resolve => setTimeout(resolve, 20));
    throw new Error('connection reset');
  };

  assert.equal(await shutdownProcess(proc, stopOptions(proc, {requestStop})), 'stopped');
  assert.deepEqual(proc.signals, []);
});

test('terminates a process that ignores the stop request, after the stop request', async () => {
  const events = [];
  const proc = new FakeProcess({exitsOn: ['SIGTERM'], events});

  const outcome = await shutdownProcess(proc, stopOptions(proc));

  assert.equal(outcome, 'terminated');
  assert.deepEqual(events, ['stop request', 'signal SIGTERM']);
});

test('goes on at once when the stop request cannot be delivered', async () => {
  const proc = new FakeProcess({exitsOn: ['SIGTERM']});
  const requestStop = async () => {
    throw new Error('connect ECONNREFUSED');
  };
  const warnings = [];

  const started = Date.now();
  const outcome = await shutdownProcess(
    proc,
    stopOptions(proc, {requestStop, stopTimeoutMs: 10_000, log: {warn: message => warnings.push(message)}})
  );

  assert.equal(outcome, 'terminated');
  assert.ok(Date.now() - started < 2000, 'must not wait out the stop timeout');
  assert.match(warnings[0], /ECONNREFUSED/);
});

test('a stop request that throws counts as undeliverable', async () => {
  const proc = new FakeProcess({exitsOn: ['SIGTERM']});
  const requestStop = () => {
    throw new Error('not connected');
  };

  assert.equal(await shutdownProcess(proc, stopOptions(proc, {requestStop})), 'terminated');
});

test('does not wait longer than the stop timeout for the stop request', async () => {
  const proc = new FakeProcess({exitsOn: ['SIGTERM']});
  const requestStop = () => new Promise(() => undefined);

  const started = Date.now();
  const outcome = await shutdownProcess(proc, stopOptions(proc, {requestStop}));

  assert.equal(outcome, 'terminated');
  assert.ok(Date.now() - started < 2000);
});

test('kills a process that ignores the termination request', async () => {
  const events = [];
  const proc = new FakeProcess({exitsOn: ['SIGKILL'], events});

  const outcome = await shutdownProcess(proc, stopOptions(proc));

  assert.equal(outcome, 'killed');
  assert.deepEqual(events, ['stop request', 'signal SIGTERM', 'signal SIGKILL']);
});

test('reports a process that survives the kill', async () => {
  const proc = new FakeProcess();

  assert.equal(await shutdownProcess(proc, stopOptions(proc)), 'timeout');
  assert.deepEqual(proc.signals, ['SIGTERM', 'SIGKILL']);
});

test('skipping the termination request goes from the stop request to the kill', async () => {
  const events = [];
  const proc = new FakeProcess({exitsOn: ['SIGKILL'], events});

  const outcome = await shutdownProcess(proc, stopOptions(proc, {skipTerminate: true}));

  assert.equal(outcome, 'killed');
  assert.deepEqual(events, ['stop request', 'signal SIGKILL']);
});

test('skipping the termination request still lets a quick stop finish cleanly', async () => {
  const proc = new FakeProcess();
  const requestStop = async () => {
    setTimeout(() => proc.exit(0), 5);
  };

  const outcome = await shutdownProcess(proc, stopOptions(proc, {requestStop, skipTerminate: true}));

  assert.equal(outcome, 'stopped');
  assert.deepEqual(proc.signals, []);
});

test('on Windows the process is asked to close instead of being signalled', async () => {
  const proc = new FakeProcess();
  const closed = [];
  const closeWindowsProcess = async pid => {
    closed.push(pid);
    setTimeout(() => proc.exit(0), 5);
    return true;
  };

  const outcome = await shutdownProcess(proc, stopOptions(proc, {platform: 'win32', closeWindowsProcess}));

  assert.equal(outcome, 'terminated');
  assert.deepEqual(closed, [4242]);
  assert.deepEqual(proc.signals, []);
});

test('on Windows a process that cannot be asked to close is killed without waiting for it', async () => {
  const events = [];
  const proc = new FakeProcess({exitsOn: ['SIGKILL'], events});

  const started = Date.now();
  const outcome = await shutdownProcess(
    proc,
    stopOptions(proc, {platform: 'win32', terminateTimeoutMs: 10_000, closeWindowsProcess: async () => false})
  );

  assert.equal(outcome, 'killed');
  assert.ok(Date.now() - started < 2000, 'must not wait out the terminate timeout');
  assert.deepEqual(events, ['stop request', 'signal SIGKILL']);
});

test('on Windows a process that ignores the close request is killed after the timeout', async () => {
  const proc = new FakeProcess({exitsOn: ['SIGKILL']});

  const outcome = await shutdownProcess(
    proc,
    stopOptions(proc, {platform: 'win32', closeWindowsProcess: async () => true})
  );

  assert.equal(outcome, 'killed');
  assert.deepEqual(proc.signals, ['SIGKILL']);
});

function fakeSystem(pids, exitsOnTerm) {
  const alive = new Set(pids);
  const sent = [];
  const sendSignal = (pid, signal) => {
    if (signal !== 0) sent.push(`${signal} ${pid}`);
    if (!alive.has(pid)) throw Object.assign(new Error('no such process'), {code: 'ESRCH'});
    if (signal === 'SIGKILL' || (signal === 'SIGTERM' && exitsOnTerm.has(pid))) alive.delete(pid);
  };
  return {alive, sent, sendSignal};
}

test('stray processes get a termination request and are killed only if they outlast it', async () => {
  const system = fakeSystem([10, 11], new Set([10]));

  await terminatePids([10, 11], {timeoutMs: 50, pollMs: 5, sendSignal: system.sendSignal});

  assert.deepEqual(system.sent, ['SIGTERM 10', 'SIGTERM 11', 'SIGKILL 11']);
  assert.equal(system.alive.size, 0);
});

test('stray processes that exit are left alone and the wait ends early', async () => {
  const system = fakeSystem([10], new Set([10]));

  const started = Date.now();
  await terminatePids([10], {timeoutMs: 10_000, pollMs: 5, sendSignal: system.sendSignal});

  assert.deepEqual(system.sent, ['SIGTERM 10']);
  assert.ok(Date.now() - started < 2000);
});

test('a stray process owned by someone else counts as running until the timeout', async () => {
  const sent = [];
  const sendSignal = (pid, signal) => {
    sent.push(`${signal} ${pid}`);
    throw Object.assign(new Error('operation not permitted'), {code: 'EPERM'});
  };

  await terminatePids([10], {timeoutMs: 20, pollMs: 5, sendSignal});

  assert.deepEqual([sent[0], sent.at(-1)], ['SIGTERM 10', 'SIGKILL 10']);
});
