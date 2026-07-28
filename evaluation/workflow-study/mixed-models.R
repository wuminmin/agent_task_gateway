#!/usr/bin/env Rscript
# Preregistered secondary models for the participant-free benchmark. The Python
# analysis remains authoritative for paired contrasts and Pareto summaries.

suppressPackageStartupMessages(library(lme4))

args <- commandArgs(trailingOnly = TRUE)
if (length(args) != 1) {
  stop("usage: mixed-models.R scored-runs.csv")
}

runs <- read.csv(args[[1]], stringsAsFactors = TRUE)
budgeted <- subset(runs, phase == "budget_level")
budgeted$budget_level_factor <- factor(budgeted$budget_level, levels = c(0.25, 0.5, 0.75, 1.0))

# Replicate labels do not identify shared API randomness across arms. They are
# independent conversations within a task-policy-level cell, so the task is the
# inferential cluster and replicate variation remains in the observation-level
# residual rather than a synthetic paired random effect.
quality <- lmer(
  answer_score ~ arm * budget_level_factor + domain + difficulty +
    (1 | task_id),
  data = budgeted
)
completion <- glmer(
  answer_task_complete ~ arm * budget_level_factor + domain + difficulty +
    (1 | task_id),
  family = binomial(link = "logit"), data = budgeted
)

print(summary(quality))
print(summary(completion))
