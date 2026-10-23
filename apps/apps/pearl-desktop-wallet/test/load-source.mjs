import {readFileSync} from 'node:fs';
import {createRequire} from 'node:module';
import {dirname, resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import ts from 'typescript';

const appDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');

// Loads TypeScript sources of the app without building it. Requests listed in `mocks` are answered with the mock, other
// relative ones are loaded from source and the rest from node_modules.
export function loadSource(mocks = {}) {
  const modules = new Map();
  const load = filename => {
    if (modules.has(filename)) return modules.get(filename).exports;
    const module = {exports: {}};
    modules.set(filename, module);
    const {outputText} = ts.transpileModule(readFileSync(filename, 'utf8'), {
      compilerOptions: {module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, esModuleInterop: true},
    });
    const require = request => {
      if (Object.hasOwn(mocks, request)) return mocks[request];
      if (request.startsWith('.')) return load(resolve(dirname(filename), `${request}.ts`));
      return createRequire(filename)(request);
    };
    new Function('require', 'module', 'exports', outputText)(require, module, module.exports);
    return module.exports;
  };
  return path => load(resolve(appDir, path));
}
