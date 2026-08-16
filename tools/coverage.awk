NR == 1 { next }

{
  split($1, location, ":")
  file = location[1]
  key = $1 " " $2
  statements[key] = $2
  files[key] = file
  if ($3 > 0) {
    hit[key] = 1
  }
}

function belongs(file, group) {
  if (group == "authentication") return index(file, "/internal/auth/") > 0
  if (group == "authorization") return index(file, "/internal/drive/") > 0
  if (group == "path") return index(file, "/internal/domain/") > 0
  if (group == "token") return index(file, "/internal/secret/") > 0
  if (group == "capability" || group == "scope-mapping") return index(file, "/internal/provider/memory/") > 0
  if (group == "state-CAS") return index(file, "/internal/state/") > 0 && index(file, "/internal/state/statecontract/") == 0
  if (group == "theme-validation") return index(file, "/internal/theme/") > 0
  if (group == "configuration") return index(file, "/internal/config/") > 0
  return 0
}

function percentage(covered, total) {
  if (total == 0) return 0
  return 100 * covered / total
}

END {
  groups[1] = "authentication"
  groups[2] = "authorization"
  groups[3] = "path"
  groups[4] = "token"
  groups[5] = "capability"
  groups[6] = "state-CAS"
  groups[7] = "scope-mapping"
  groups[8] = "theme-validation"
  groups[9] = "configuration"

  for (key in statements) {
    repo_total += statements[key]
    if (hit[key]) repo_covered += statements[key]
    for (index_value = 1; index_value <= 9; index_value++) {
      group = groups[index_value]
      if (belongs(files[key], group)) {
        group_total[group] += statements[key]
        if (hit[key]) group_covered[group] += statements[key]
      }
    }
  }

  failed = 0
  repo_percentage = percentage(repo_covered, repo_total)
  printf "repository coverage: %.3f%% (%d/%d; required >= 85%%)\n", repo_percentage, repo_covered, repo_total
  if (repo_percentage + 0.000001 < 85) failed = 1

  for (index_value = 1; index_value <= 9; index_value++) {
    group = groups[index_value]
    group_percentage = percentage(group_covered[group], group_total[group])
    printf "%s coverage: %.3f%% (%d/%d; required >= 95%%)\n", group, group_percentage, group_covered[group], group_total[group]
    if (group_total[group] == 0 || group_percentage + 0.000001 < 95) failed = 1
  }
  exit failed
}
