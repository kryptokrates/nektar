"""Directory-plugin entrypoint for Hermes (`~/.hermes/plugins/memory/nektar`)."""

from .hermes_memory_nektar import NektarMemoryProvider, register

__all__ = ["NektarMemoryProvider", "register"]
