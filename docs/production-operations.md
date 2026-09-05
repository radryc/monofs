# Production Operations Playbook

## Purpose

This guide focuses on the production development and rollout cycle for a MonoFS-backed workspace:

- edit source and Guardian intent from the existing mounted workspace
- publish changes deliberately
- validate behavior with Doctor and monitoring
- respond to rollout failures
- roll back safely

It also includes supporting notes for cluster access and temporary port-forward workflows when needed.

