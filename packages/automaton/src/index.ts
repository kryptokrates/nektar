// @nektar/automaton � public surface: pure automaton core + the on-chain
// AutomatonChain (chain.ts imports core � never the other way around).
export * from './core.js'
export { AutomatonChain, decodeNektarAutomaton, type AutomatonHead, type DecodedAutomaton } from './chain.js'
