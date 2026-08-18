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
  if (group == "capability") return file ~ /\/internal\/provider\/memory\/transfer.go$/ || file ~ /\/internal\/portable\/transfers.go$/ || file ~ /\/internal\/objectstore\/gcs\/transfers.go$/
  if (group == "state-CAS") return (index(file, "/internal/state/") > 0 && index(file, "/internal/state/statecontract/") == 0) || file ~ /\/internal\/portable\/state.go$/
  if (group == "scope-mapping") return index(file, "/internal/provider/memory/") > 0 || file ~ /\/internal\/portable\/(filesystem|operations|state|transfers).go$/
  if (group == "canonical-format-key-version-checkpoint") return index(file, "/internal/storageformat/") > 0 || file ~ /\/internal\/portable\/(engine|checkpoint).go$/
  if (group == "write-gate-admission") return file ~ /\/internal\/portable\/(gate|admission).go$/
  if (group == "operation-fencing-recovery") return file ~ /\/internal\/portable\/(gate|operations).go$/
  if (group == "directory-manifest") return file ~ /\/internal\/portable\/filesystem.go$/
  if (group == "gcs-transport") return index(file, "/internal/objectstore/gcs/") > 0
  if (group == "theme-validation") return index(file, "/internal/theme/") > 0
  if (group == "configuration") return index(file, "/internal/config/") > 0
  if (group == "preview-core") return index(file, "/internal/preview/") > 0 && index(file, "/internal/preview/imagegen/") == 0 && index(file, "/internal/preview/memory/") == 0 && index(file, "/internal/preview/durable/") == 0 && index(file, "/internal/preview/storecontract/") == 0
  if (group == "preview-image-generator") return index(file, "/internal/preview/imagegen/") > 0
  if (group == "preview-store") return index(file, "/internal/preview/memory/") > 0 || index(file, "/internal/preview/durable/") > 0
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
  groups[8] = "canonical-format-key-version-checkpoint"
  groups[9] = "write-gate-admission"
  groups[10] = "operation-fencing-recovery"
  groups[11] = "directory-manifest"
  groups[12] = "gcs-transport"
  groups[13] = "theme-validation"
  groups[14] = "configuration"
  groups[15] = "preview-core"
  groups[16] = "preview-image-generator"
  groups[17] = "preview-store"
  group_count = 17

  for (key in statements) {
    repo_total += statements[key]
    if (hit[key]) repo_covered += statements[key]
    for (index_value = 1; index_value <= group_count; index_value++) {
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

  for (index_value = 1; index_value <= group_count; index_value++) {
    group = groups[index_value]
    group_percentage = percentage(group_covered[group], group_total[group])
    printf "%s coverage: %.3f%% (%d/%d; required >= 95%%)\n", group, group_percentage, group_covered[group], group_total[group]
    if (group_total[group] == 0 || group_percentage + 0.000001 < 95) failed = 1
  }
  exit failed
}
