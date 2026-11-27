<svelte:options namespace="svg" />

<script lang="ts">
  interface Props {
    uid: string
  }

  let { uid }: Props = $props()
</script>

<defs>
  <radialGradient id={`${uid}-glow-l`} cx="14%" cy="42%" r="40%">
    <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.12" />
    <stop offset="100%" stop-color="#4cc26b" stop-opacity="0" />
  </radialGradient>
  <radialGradient id={`${uid}-glow-r`} cx="86%" cy="42%" r="40%">
    <stop offset="0%" stop-color="#f7991b" stop-opacity="0.13" />
    <stop offset="100%" stop-color="#f7991b" stop-opacity="0" />
  </radialGradient>
  <radialGradient id={`${uid}-glow-c`} cx="50%" cy="42%" r="22%">
    <stop offset="0%" stop-color="#f7991b" stop-opacity="0.18" />
    <stop offset="60%" stop-color="#4cc26b" stop-opacity="0.05" />
    <stop offset="100%" stop-color="#090807" stop-opacity="0" />
  </radialGradient>
  <linearGradient id={`${uid}-hex`} x1="0%" y1="0%" x2="0%" y2="100%">
    <stop offset="0%" stop-color="#191613" />
    <stop offset="100%" stop-color="#0f0e0c" />
  </linearGradient>
  <linearGradient id={`${uid}-bar-l`} x1="0%" y1="0%" x2="100%" y2="0%">
    <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.08" />
    <stop offset="100%" stop-color="#4cc26b" stop-opacity="0.42" />
  </linearGradient>
  <linearGradient id={`${uid}-bar-r`} x1="100%" y1="0%" x2="0%" y2="0%">
    <stop offset="0%" stop-color="#f7991b" stop-opacity="0.08" />
    <stop offset="100%" stop-color="#f7991b" stop-opacity="0.44" />
  </linearGradient>
  <!-- userSpaceOnUse guarantees crisp strokes even on purely horizontal single-row paths -->
  <linearGradient id={`${uid}-flow-l`} gradientUnits="userSpaceOnUse" x1="250" y1="0" x2="380" y2="0">
    <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.24" />
    <stop offset="100%" stop-color="#4cc26b" stop-opacity="0.94" />
  </linearGradient>
  <linearGradient id={`${uid}-flow-r`} gradientUnits="userSpaceOnUse" x1="540" y1="0" x2="670" y2="0">
    <stop offset="0%" stop-color="#f7991b" stop-opacity="0.94" />
    <stop offset="100%" stop-color="#f7991b" stop-opacity="0.26" />
  </linearGradient>

  <marker id={`${uid}-arr-l`} viewBox="0 0 10 10" refX="7" refY="5" markerWidth="6.5" markerHeight="6.5" orient="auto">
    <path d="M 2 2 Q 5.2 4.3 7.5 5 Q 5.2 5.7 2 8" fill="none" stroke="#4cc26b" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
  </marker>
  <marker id={`${uid}-arr-r`} viewBox="0 0 10 10" refX="7" refY="5" markerWidth="6.5" markerHeight="6.5" orient="auto">
    <path d="M 2 2 Q 5.2 4.3 7.5 5 Q 5.2 5.7 2 8" fill="none" stroke="#f7991b" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
  </marker>
  <marker id={`${uid}-arr-l-dim`} viewBox="0 0 10 10" refX="7" refY="5" markerWidth="6" markerHeight="6" orient="auto">
    <path d="M 2 2 Q 5.2 4.3 7.5 5 Q 5.2 5.7 2 8" fill="none" stroke="#4cc26b" stroke-opacity="0.48" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
  </marker>
  <marker id={`${uid}-arr-r-dim`} viewBox="0 0 10 10" refX="7" refY="5" markerWidth="6" markerHeight="6" orient="auto">
    <path d="M 2 2 Q 5.2 4.3 7.5 5 Q 5.2 5.7 2 8" fill="none" stroke="#f7991b" stroke-opacity="0.48" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
  </marker>

  <filter id={`${uid}-neon`} x="-40%" y="-40%" width="180%" height="180%">
    <feGaussianBlur stdDeviation="2.8" result="blur" />
    <feMerge>
      <feMergeNode in="blur" />
      <feMergeNode in="SourceGraphic" />
    </feMerge>
  </filter>
</defs>
