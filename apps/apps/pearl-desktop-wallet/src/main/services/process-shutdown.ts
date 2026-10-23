import {spawn} from 'child_process';
import type {ChildProcess} from 'child_process';

type ShutdownOutcome = 'already-exited' | 'stopped' | 'terminated' | 'killed' | 'timeout';

interface ShutdownLogger {
  warn: (message: string) => void;
}

interface ShutdownOptions {
  // Asks the process to exit by itself. A rejection means the request could not be delivered.
  requestStop: () => Promise<unknown>;
  // Covers the stop request and the wait for the exit that follows it.
  stopTimeoutMs: number;
  terminateTimeoutMs: number;
  killTimeoutMs: number;
  // Skips the termination request for callers that can't wait for a process that is slow to exit.
  skipTerminate?: boolean;
  platform?: NodeJS.Platform;
  // Asks a Windows process to close. Resolves true if the request was delivered.
  closeWindowsProcess?: (pid: number) => Promise<boolean>;
  log?: ShutdownLogger;
}

const silentLogger: ShutdownLogger = {warn: () => undefined};

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function hasExited(proc: ChildProcess): boolean {
  return proc.exitCode !== null || proc.signalCode !== null;
}

function waitForExit(proc: ChildProcess, timeoutMs: number): Promise<boolean> {
  if (hasExited(proc)) {
    return Promise.resolve(true);
  }

  return new Promise<boolean>(resolve => {
    const onExit = () => {
      clearTimeout(timer);
      resolve(true);
    };
    const timer = setTimeout(
      () => {
        proc.off('exit', onExit);
        resolve(false);
      },
      Math.max(timeoutMs, 0)
    );
    proc.once('exit', onExit);
  });
}

// Resolves false if the request fails or isn't answered in time.
async function deliverStop(options: ShutdownOptions, log: ShutdownLogger, pid: number | undefined): Promise<boolean> {
  let timer: NodeJS.Timeout | undefined;
  const timedOut = new Promise<boolean>(resolve => {
    timer = setTimeout(() => resolve(false), options.stopTimeoutMs);
  });
  const request = options.requestStop().then(
    () => true,
    error => {
      log.warn(`Could not ask process ${pid} to stop: ${errorMessage(error)}`);
      return false;
    }
  );

  try {
    return await Promise.race([request, timedOut]);
  } finally {
    clearTimeout(timer);
  }
}

// taskkill without /F asks the process to close its windows. Console programs have none, for which it fails at
// once, so the exit code tells whether waiting for the process is worthwhile.
function closeWindowsProcess(pid: number): Promise<boolean> {
  return new Promise<boolean>(resolve => {
    const taskkill = spawn('taskkill', ['/PID', String(pid), '/T'], {stdio: 'ignore', windowsHide: true});
    taskkill.once('error', () => resolve(false));
    taskkill.once('exit', code => resolve(code === 0));
  });
}

function signalProcess(proc: ChildProcess, signal: NodeJS.Signals, log: ShutdownLogger): void {
  try {
    proc.kill(signal);
  } catch (error) {
    log.warn(`Failed to send ${signal} to process ${proc.pid}: ${errorMessage(error)}`);
  }
}

// Every signal sent through a child handle on Windows is a hard kill, so there the stop request is the only clean
// way out, and the process is killed only once that and the termination request have failed.
async function shutdownProcess(proc: ChildProcess, options: ShutdownOptions): Promise<ShutdownOutcome> {
  const platform = options.platform ?? process.platform;
  const log = options.log ?? silentLogger;

  if (hasExited(proc)) {
    return 'already-exited';
  }

  const stopDeadline = Date.now() + options.stopTimeoutMs;
  const stopDelivered = await deliverStop(options, log, proc.pid);

  // Without a delivered request there is nothing to wait for.
  if (await waitForExit(proc, stopDelivered ? stopDeadline - Date.now() : 0)) {
    return 'stopped';
  }

  if (!options.skipTerminate) {
    log.warn(`Process ${proc.pid} did not exit after being asked to stop, terminating it`);

    let terminationRequested = true;
    if (platform === 'win32') {
      terminationRequested = await (options.closeWindowsProcess ?? closeWindowsProcess)(proc.pid ?? -1);
    } else {
      signalProcess(proc, 'SIGTERM', log);
    }

    if (await waitForExit(proc, terminationRequested ? options.terminateTimeoutMs : 0)) {
      return 'terminated';
    }
  }

  log.warn(`Process ${proc.pid} is still running, killing it`);
  signalProcess(proc, 'SIGKILL', log);

  return (await waitForExit(proc, options.killTimeoutMs)) ? 'killed' : 'timeout';
}

interface TerminatePidsOptions {
  timeoutMs: number;
  pollMs?: number;
  // process.kill, replaceable for tests.
  sendSignal?: (pid: number, signal: NodeJS.Signals | 0) => void;
  log?: ShutdownLogger;
}

// For processes this app didn't start. They have no handle to wait on, and the credentials to ask them to stop
// aren't known, so they get a termination request and are killed only if they outlast the timeout.
async function terminatePids(pids: number[], options: TerminatePidsOptions): Promise<void> {
  const sendSignal = options.sendSignal ?? ((pid, signal) => process.kill(pid, signal));
  const log = options.log ?? silentLogger;
  const pollMs = options.pollMs ?? 100;

  const isAlive = (pid: number) => {
    try {
      sendSignal(pid, 0);
      return true;
    } catch (error) {
      // EPERM means the process exists but belongs to someone else.
      return (error as NodeJS.ErrnoException).code === 'EPERM';
    }
  };
  const signalAll = (targets: number[], signal: NodeJS.Signals) => {
    for (const pid of targets) {
      try {
        sendSignal(pid, signal);
      } catch (error) {
        log.warn(`Failed to send ${signal} to process ${pid}: ${errorMessage(error)}`);
      }
    }
  };

  signalAll(pids, 'SIGTERM');

  const deadline = Date.now() + options.timeoutMs;
  while (pids.some(isAlive) && Date.now() < deadline) {
    await new Promise(resolve => setTimeout(resolve, pollMs));
  }

  const survivors = pids.filter(isAlive);
  if (survivors.length > 0) {
    log.warn(`Processes ${survivors.join(', ')} are still running, killing them`);
    signalAll(survivors, 'SIGKILL');
  }
}

export {shutdownProcess, terminatePids};
export type {ShutdownLogger, ShutdownOptions, ShutdownOutcome, TerminatePidsOptions};
