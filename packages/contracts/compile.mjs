import {compile} from "runar-compiler";
import fs from "node:fs";
for (const name of ["MamemAnchor","MamemAutomaton","MamemAutomatonZk","MamemFeeVault","MamemCheckpointBond","DisclosureSale"]) {
  const r=compile(fs.readFileSync(`src/${name}.runar.ts`,"utf8"));
  if(!r.success){console.error(name,JSON.stringify(r.diagnostics));process.exit(1)}
  const out=JSON.stringify(r.artifact,(k,v)=>typeof v==="bigint"?v.toString():v,2);
  const file=`${name}.json`;
  const prev=fs.existsSync(file)?fs.readFileSync(file,"utf8"):null;
  if(prev&&prev!==out){fs.writeFileSync(file,out);console.log(name,"artifact regenerated")}else if(!prev){fs.writeFileSync(file,out);console.log(name,"artifact written")}else console.log(name,"artifact unchanged");
}
