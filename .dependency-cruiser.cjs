/** @type {import('dependency-cruiser').IConfiguration} */
module.exports = {
  forbidden: [
    {
      name: 'no-circular',
      severity: 'error',
      comment: 'no RUNTIME dependency cycles (type-only edges are erased by tsc — `import type` is the fix for barrel cycles)',
      from: {},
      to: { circular: true, dependencyTypesNot: ['type-only'] }
    },
    {
      name: 'no-orphans',
      severity: 'warn',
      comment: 'every module must be reachable (bin/test entries excluded below)',
      from: {
        orphan: true,
        pathNot: [
          // test files, bins, scripts and generated entry points are roots
          '(^|/)(test|scripts|demo|bin|fixtures?)/',
          '\\.(test|spec|d)\\.[cm]?[jt]s$',
          '/(bin|cli)\\.[cm]?[jt]s$',
          // .runar.ts = smart-contract sources — compiled by runar-cli into
          // JSON artifacts, never imported by the module graph
          '\\.runar\\.[cm]?[jt]s$',
          // library/CLI entrypoints invoked from outside this codebase
          'payclient\\.[cm]?[jt]s$',      // standalone x402 payer client
          'announce\\.mjs$'               // overlay announcement CLI
        ]
      },
      to: {}
    },
    {
      name: 'not-to-dev-dep',
      severity: 'error',
      comment: 'src/ must not import devDependencies (they do not ship)',
      from: { pathNot: '(^|/)(test|scripts|demo)/' },
      to: { dependencyTypes: ['npm-dev'] }
    },
    {
      name: 'src-not-to-test',
      severity: 'error',
      comment: 'source code must not import test files',
      from: { pathNot: '(^|/)(test|scripts|demo)/' },
      to: { path: '(^|/)test/' }
    },
    {
      name: 'not-to-unresolvable',
      severity: 'error',
      comment: 'every import must resolve',
      from: {},
      to: {
        couldNotResolve: true,
        // couldNotResolve deps keep the RAW specifier (../../Foo.json) — so
        // match the spec suffix, not the repo path. These are generated
        // artifacts: compile:contract → packages/contracts/Nektar*.json and
        // stark:build → packages/zk/stark/verifier.js (gitignored by design)
        pathNot: [
          '(Nektar|DisclosureSale)[A-Za-z0-9]*\\.json$',
          'stark/verifier\\.(js|wasm)$'
        ]
      }
    },
    {
      name: 'no-duplicate-deps',
      severity: 'warn',
      comment: 'a dep must not be both a dependency and a devDependency',
      from: {},
      to: { moreThanOneDependencyType: true }
    },
    {
      // recommended: deprecated node core modules must not be imported
      name: 'no-deprecated-core',
      severity: 'error',
      comment: 'deprecated node core module — node does not deprecate lightly',
      from: {},
      to: {
        dependencyTypes: ['core'],
        path: '^(?:punycode|domain|constants|sys|_linklist|_stream_wrap)$'
      }
    },
    {
      // recommended: an npm import with no package.json entry at all —
      // check-deps.mjs catches the same thing by source scan; this catches
      // it through the resolved graph (redundant depth is intentional)
      name: 'no-non-package-json',
      severity: 'error',
      comment: 'npm package not declared in any package.json — add it or stop importing it',
      from: {},
      to: { dependencyTypes: ['npm-no-pkg', 'npm-unknown'] }
    },
    {
      name: 'no-node_modules-traversal',
      severity: 'error',
      comment: 'import packages via their exports — a literal ../node_modules/ path only works by hoisting accident (tools/check-deps.mjs guards this too)',
      from: { path: 'node_modules' },
      to: { pathNot: 'node_modules' }
    },
    {
      name: 'not-to-unresolvable-non-package',
      severity: 'warn',
      comment: 'imports must come from a declared dependency (complements tools/check-deps.mjs)',
      from: { pathNot: '(^|/)(test|scripts|demo)/' },
      to: { dependencyTypes: ['npm-optional', 'npm-peer'] }
    },
    {
      name: 'not-to-deprecated',
      severity: 'warn',
      comment: 'warn on deprecated npm packages in the graph',
      from: {},
      to: { dependencyTypes: ['deprecated'] }
    }
  ],
  options: {
    doNotFollow: {
      path: 'node_modules',
      // recommended preset: also don't FOLLOW npm edges into their own
      // dep trees — we audit our imports, not our deps' internals
      dependencyTypes: ['npm', 'npm-dev', 'npm-optional', 'npm-peer', 'npm-bundled', 'npm-no-pkg']
    },
    exclude: { path: '(^|/)dist/' },
    tsPreCompilationDeps: true,
    enhancedResolveOptions: {
      exportsFields: ['exports'],
      conditionNames: ['import', 'require', 'node', 'default', 'types'],
      mainFields: ['module', 'main', 'types']
    },
    reporterOptions: {
      text: { highlightFocused: true },
      err: { }
    },
    prefix: 'https://github.com/nedzof/nektar/blob/master/'
  }
}
