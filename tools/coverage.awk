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
  if (group == "capability") return file ~ /\/internal\/provider\/memory\/transfer.go$/ || file ~ /\/internal\/portable\/transfers(008)?\.go$/ || file ~ /\/internal\/objectstore\/gcs\/transfers.go$/
  if (group == "state-CAS") return (index(file, "/internal/state/") > 0 && index(file, "/internal/state/statecontract/") == 0) || file ~ /\/internal\/portable\/(state|domain_store|domain_tree|domain_tree_stream).go$/
  if (group == "scope-mapping") return index(file, "/internal/provider/memory/") > 0 || file ~ /\/internal\/portable\/(state|runtime008).go$/
  if (group == "canonical-format-key-version-checkpoint") return index(file, "/internal/storageformat/") > 0 || file ~ /\/internal\/portable\/(engine|checkpoint|checkpoint008|checkpoint_reachability|checkpoint_visit_set).go$/
  if (group == "gate-catalog-domain-freeze") return file ~ /\/internal\/portable\/(gate|domain_catalog|checkpoint008).go$/
  if (group == "domain-publication-lost-success") return file ~ /\/internal\/portable\/(domain_store|domain_tree|domain_tree_stream).go$/
  if (group == "namespace-tree") return file ~ /\/internal\/portable\/(runtime008|namespace_store|namespace_batch|namespace_trash|namespace_projection).go$/
  if (group == "gcs-transport") return index(file, "/internal/objectstore/gcs/") > 0
  if (group == "theme-validation") return index(file, "/internal/theme/") > 0
  if (group == "configuration") return index(file, "/internal/config/") > 0
  if (group == "preview-core") return index(file, "/internal/preview/") > 0 && index(file, "/internal/preview/imagegen/") == 0 && index(file, "/internal/preview/memory/") == 0 && index(file, "/internal/preview/durable/") == 0 && index(file, "/internal/preview/storecontract/") == 0
  if (group == "preview-image-generator") return index(file, "/internal/preview/imagegen/") > 0
  if (group == "preview-store") return index(file, "/internal/preview/memory/") > 0 || index(file, "/internal/preview/durable/") > 0
  if (group == "migration") return file ~ /\/internal\/portable\/migration(008|_ledger)?\.go$/
  return 0
}

function percentage(covered, total) {
  if (total == 0) return 0
  return 100 * covered / total
}

function required_percentage(group) {
  if (group == "migration") return 98
  return 95
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
  groups[9] = "gate-catalog-domain-freeze"
  groups[10] = "domain-publication-lost-success"
  groups[11] = "namespace-tree"
  groups[12] = "gcs-transport"
  groups[13] = "theme-validation"
  groups[14] = "configuration"
  groups[15] = "preview-core"
  groups[16] = "preview-image-generator"
  groups[17] = "preview-store"
  groups[18] = "migration"
  group_count = 18

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
  if (only_group == "") {
    repo_percentage = percentage(repo_covered, repo_total)
    printf "repository coverage: %.3f%% (%d/%d; required >= 85%%)\n", repo_percentage, repo_covered, repo_total
    if (repo_percentage + 0.000001 < 85) failed = 1
  }

  for (index_value = 1; index_value <= group_count; index_value++) {
    group = groups[index_value]
    if (only_group != "" && group != only_group) continue
    group_percentage = percentage(group_covered[group], group_total[group])
    required = required_percentage(group)
    printf "%s coverage: %.3f%% (%d/%d; required >= %d%%)\n", group, group_percentage, group_covered[group], group_total[group], required
    if (group_total[group] == 0 || group_percentage + 0.000001 < required) failed = 1
  }
  exit failed
}
