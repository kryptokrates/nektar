permit(principal is Agent, action == Action::"read", resource is Fragment)
when { resource.level <= principal.level + 1 };

permit(principal is Agent, action == Action::"disclose", resource is Fragment)
when { resource.level <= principal.level };

permit(principal is Agent, action in [Action::"write", Action::"rotate", Action::"revoke"], resource is Fragment)
when { resource.shard == "ops" };
