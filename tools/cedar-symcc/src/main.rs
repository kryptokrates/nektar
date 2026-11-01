// POL-015: symbolic proof (Cedar SymCC + cvc5) that the instance policy never
// lets a principal read/disclose a fragment above its level, for EVERY typed
// request — not a bounded sample. A mutant policy must be refuted.
//   usage: cedar-symcc-check <schema> <policies> <spec>   exit 0 = property holds
use cedar_policy::{PolicySet, Schema};
use cedar_policy_symcc::{solver::LocalSolver, CedarSymCompiler, SymEnv, WellTypedPolicies};
use std::str::FromStr;

#[tokio::main]
async fn main() {
    let a: Vec<String> = std::env::args().collect();
    let read = |p: &str| std::fs::read_to_string(p).unwrap_or_else(|e| panic!("{p}: {e}"));
    let (schema, _) = Schema::from_cedarschema_str(&read(&a[1])).expect("schema");
    let policies = PolicySet::from_str(&read(&a[2])).expect("policies");
    let spec = PolicySet::from_str(&read(&a[3])).expect("spec");
    let mut compiler = CedarSymCompiler::new(LocalSolver::cvc5().expect("cvc5 (set CVC5)")).expect("compiler");
    let (mut checked, mut holds) = (0, true);
    for env in schema.request_envs() {
        let action = env.action().to_string();
        if !(action.contains("\"read\"") || action.contains("\"disclose\"")) { continue }
        let sym = SymEnv::new(&schema, &env).expect("symenv");
        let p = WellTypedPolicies::from_policies(&policies, &env, &schema).expect("typecheck policies");
        let q = WellTypedPolicies::from_policies(&spec, &env, &schema).expect("typecheck spec");
        let ok = compiler.check_implies(&p, &q, &sym).await.expect("solver");
        checked += 1;
        println!("{} {} {} -> {}", env.principal(), action, env.resource(), if ok { "PROVED" } else { "REFUTED" });
        if !ok {
            holds = false;
            if let Ok(Some(cex)) = compiler.check_implies_with_counterexample(&p, &q, &sym).await {
                println!("  counterexample: {:?}", cex.request);
            }
        }
    }
    println!("request environments checked: {checked}; level isolation {}", if holds { "HOLDS" } else { "VIOLATED" });
    std::process::exit(if holds && checked > 0 { 0 } else { 1 });
}
