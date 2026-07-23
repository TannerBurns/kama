(.items | length) == 1 and
(.items[0] as $pod |
  ($pod.metadata.uid | type == "string" and length > 0) and
  ($pod.spec.nodeName | type == "string" and length > 0) and
  (if $runtimeClass == "" then
    $pod.spec.runtimeClassName == null
  else
    $pod.spec.runtimeClassName == $runtimeClass
  end) and
  any($pod.status.conditions[]; .type == "Ready" and .status == "True") and
  any($pod.status.containerStatuses[];
    .name == "runtime" and .ready == true and
    .restartCount == 0 and .imageID == $image))
