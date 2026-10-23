import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {test} from 'node:test';
import {loadSource} from './load-source.mjs';

class FakeOyster extends EventEmitter {
  constructor({exitsOn = []} = {}) {
    super();
    this.pid = 4242;
    this.exitCode = null;
    this.signalCode = null;
    this.signals = [];
    this.exitsOn = new Set(exitsOn);
    this.stdout = new EventEmitter();
    this.stderr = new EventEmitter();
  }

  kill(signal) {
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

// Builds a WalletProcess whose oyster is a FakeOyster, so no binary is needed.
function walletProcess({oyster, walletService}) {
  const spawned = [];
  const spawn = command => {
    spawned.push(command);
    if (command === 'lsof') {
      const lsof = new EventEmitter();
      lsof.stdout = new EventEmitter();
      setImmediate(() => lsof.emit('close', 1));
      return lsof;
    }
    return oyster;
  };
  const silent = {info() {}, warn() {}, error() {}, log() {}};
  const load = loadSource({
    child_process: {spawn},
    electron: {app: {isPackaged: true}},
    'electron-log': silent,
    fs: {existsSync: () => true, accessSync() {}, constants: {F_OK: 0, X_OK: 1}},
    '../config/network-config': {
      getCurrentNetworkConfig: () => ({rpcPort: 8335, walletFlag: '', dataSubdir: 'mainnet'}),
    },
    './setup-file': {},
  });
  const {WalletProcess} = load('src/main/services/wallet-process.ts');
  const config = {dataDir: '/tmp/wallet', rpcUser: 'user', rpcPassword: 'pass'};
  return {wallet: new WalletProcess(config, walletService), spawned};
}

// Electron sets resourcesPath, which is where a packaged app keeps the oyster binary.
function packagedApp(t) {
  process.resourcesPath = '/opt/wallet/resources';
  t.after(() => {
    delete process.resourcesPath;
  });
}

async function until(condition) {
  for (let waited = 0; !condition(); waited += 10) {
    assert.ok(waited < 5000, 'timed out waiting for the condition');
    await new Promise(resolve => setTimeout(resolve, 10));
  }
}

test('stop asks oyster to exit over RPC and leaves it unsignalled', async t => {
  packagedApp(t);
  const oyster = new FakeOyster();
  const stopRequests = [];
  const walletService = {
    getBalance: async () => 0,
    stopDaemon: async timeoutMs => {
      stopRequests.push(timeoutMs);
      setImmediate(() => oyster.exit(0));
    },
  };
  const {wallet} = walletProcess({oyster, walletService});

  await wallet.start();
  assert.equal(wallet.isRunning(), true);

  const result = await wallet.stop();

  assert.deepEqual(result, {success: true, message: 'Wallet stopped successfully'});
  assert.equal(stopRequests.length, 1);
  assert.deepEqual(oyster.signals, []);
  assert.equal(wallet.isRunning(), false);
  assert.equal(wallet.getStatus().pid, undefined);
});

test('stop terminates oyster when the stop request cannot be delivered', async t => {
  packagedApp(t);
  const oyster = new FakeOyster({exitsOn: ['SIGTERM']});
  const walletService = {
    getBalance: async () => 0,
    stopDaemon: async () => {
      throw new Error('Cannot connect to RPC server');
    },
  };
  const {wallet} = walletProcess({oyster, walletService});

  await wallet.start();
  const result = await wallet.stop();

  assert.equal(result.success, true);
  assert.deepEqual(oyster.signals, ['SIGTERM']);
  assert.equal(wallet.isRunning(), false);
});

test('a forced stop goes straight to the kill when oyster cannot be asked to stop', async t => {
  packagedApp(t);
  const oyster = new FakeOyster({exitsOn: ['SIGKILL']});
  const walletService = {
    getBalance: async () => 0,
    stopDaemon: async () => {
      throw new Error('Cannot connect to RPC server');
    },
  };
  const {wallet} = walletProcess({oyster, walletService});

  await wallet.start();
  const result = await wallet.stop({force: true});

  assert.deepEqual(result, {success: true, message: 'Wallet force stopped'});
  assert.deepEqual(oyster.signals, ['SIGKILL']);
});

test('stop also stops oyster that is still starting up', async t => {
  packagedApp(t);
  const oyster = new FakeOyster({exitsOn: ['SIGTERM']});
  const walletService = {
    getBalance: async () => {
      throw new Error('Cannot connect to RPC server');
    },
    stopDaemon: async () => {
      throw new Error('Cannot connect to RPC server');
    },
  };
  const {wallet, spawned} = walletProcess({oyster, walletService});

  const starting = wallet.start();
  const startFailed = assert.rejects(starting, /exited before it was ready/);
  await until(() => spawned.length > 1);
  assert.equal(wallet.isRunning(), false);
  assert.notEqual(wallet.getStatus().pid, undefined);

  const result = await wallet.stop();

  assert.equal(result.success, true);
  assert.deepEqual(oyster.signals, ['SIGTERM']);
  await startFailed;
  assert.equal(wallet.getStatus().pid, undefined);
});

test('stop reports that there is nothing to stop', async () => {
  const {wallet} = walletProcess({oyster: new FakeOyster(), walletService: {}});

  assert.deepEqual(await wallet.stop(), {success: false, message: 'Wallet is not running'});
});
