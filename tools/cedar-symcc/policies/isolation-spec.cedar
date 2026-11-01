// POL-015 property: any allowed read/disclose has resource.level <= principal.level.
// check_implies(instance ∧ action, spec) over every typed request environment.
permit(principal is Agent, action, resource is Fragment)
when { resource.level <= principal.level };

permit(principal is User, action, resource is Fragment)
when { resource.level <= principal.level };
