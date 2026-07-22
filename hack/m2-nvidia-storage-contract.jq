.schemaVersion == 1 and .mode == $mode and .storageClass == $storageClass and
.modelCache.metadata.name == "e2e-serving-nvidia-cache" and
(.modelCache.metadata.uid | type == "string" and length > 0) and
.modelCache.metadata.deletionTimestamp == null and
.modelCache.status.observedGeneration == .modelCache.metadata.generation and
.modelCache.status.storageClassName == $storageClass and
any(.modelCache.status.conditions[]?; .type == "Ready" and .status == "True") and
.modelArtifact.metadata.name == "e2e-serving-nvidia-model" and
(.modelArtifact.metadata.uid | type == "string" and length > 0) and
.modelArtifact.metadata.deletionTimestamp == null and
.modelArtifact.status.observedGeneration == .modelArtifact.metadata.generation and
.modelArtifact.spec.format == "GGUF" and
.modelArtifact.spec.entrypoint == "smollm2-360m-instruct-q8_0.gguf" and
.modelArtifact.spec.cacheRef.name == "e2e-serving-nvidia-cache" and
.modelArtifact.spec.verification.expectedSHA256 == $modelDigest and
.modelArtifact.spec.verification.expectedSize == $modelSize and
.modelArtifact.spec.source.huggingFace.repository == "HuggingFaceTB/SmolLM2-360M-Instruct-GGUF" and
.modelArtifact.spec.source.huggingFace.revision == $modelRevision and
(.modelArtifact.spec.source.huggingFace.files | index("smollm2-360m-instruct-q8_0.gguf") != null) and
.modelArtifact.status.artifactDigest == $modelDigest and
any(.modelArtifact.status.conditions[]?; .type == "Ready" and .status == "True") and
.persistentVolumeClaim.metadata.namespace == $namespace and
.persistentVolumeClaim.metadata.name == .modelCache.status.claimName and
.persistentVolumeClaim.metadata.uid == .modelCache.status.claimUID and
.persistentVolumeClaim.metadata.deletionTimestamp == null and
.persistentVolumeClaim.spec.storageClassName == $storageClass and
((.persistentVolumeClaim.spec.accessModes // []) | sort) == ["ReadWriteOnce"] and
((.persistentVolumeClaim.status.accessModes // .persistentVolumeClaim.spec.accessModes // []) | sort) == ["ReadWriteOnce"] and
.persistentVolumeClaim.spec.volumeMode == "Filesystem" and
.persistentVolumeClaim.spec.volumeName == .modelCache.status.volumeName and
.persistentVolumeClaim.status.phase == "Bound" and
.persistentVolume.metadata.name == .modelCache.status.volumeName and
.persistentVolume.metadata.uid == .modelCache.status.volumeUID and
.persistentVolume.metadata.deletionTimestamp == null and
.persistentVolume.spec.storageClassName == $storageClass and
((.persistentVolume.spec.accessModes // []) | sort) == ["ReadWriteOnce"] and
.persistentVolume.spec.volumeMode == "Filesystem" and
.persistentVolume.spec.claimRef.apiVersion == "v1" and
.persistentVolume.spec.claimRef.kind == "PersistentVolumeClaim" and
.persistentVolume.spec.claimRef.namespace == $namespace and
.persistentVolume.spec.claimRef.name == .persistentVolumeClaim.metadata.name and
.persistentVolume.spec.claimRef.uid == .persistentVolumeClaim.metadata.uid and
.persistentVolume.status.phase == "Bound" and
.modelArtifact.status.location.claimName == .persistentVolumeClaim.metadata.name and
.modelArtifact.status.location.claimUID == .persistentVolumeClaim.metadata.uid and
.modelArtifact.status.location.volumeName == .persistentVolume.metadata.name and
.modelArtifact.status.location.volumeUID == .persistentVolume.metadata.uid and
.modelArtifact.status.location.readOnly == true and
(if $mode == "existingClaim" then
  .modelCache.spec.retentionPolicy == "Retain" and
  .modelCache.spec.storage.existingClaim.name == $adoptedClaim and
  (.modelCache.spec.storage | has("claimTemplate") | not) and
  .persistentVolumeClaim.metadata.name == $adoptedClaim and
  .persistentVolumeClaim.metadata.uid == $adoptedClaimUID and
  .persistentVolume.metadata.name == $adoptedVolume and
  .persistentVolume.metadata.uid == $adoptedVolumeUID
else
  $mode == "claimTemplate" and .modelCache.spec.retentionPolicy == "Delete" and
  (.modelCache.spec.storage | has("existingClaim") | not) and
  .modelCache.spec.storage.claimTemplate.spec.storageClassName == $storageClass and
  ((.modelCache.spec.storage.claimTemplate.spec.accessModes // []) | sort) == ["ReadWriteOnce"] and
  (.modelCache.spec.storage.claimTemplate.spec.volumeMode // "Filesystem") == "Filesystem" and
  $adoptedClaim == "" and $adoptedClaimUID == "" and
  $adoptedVolume == "" and $adoptedVolumeUID == ""
end)
