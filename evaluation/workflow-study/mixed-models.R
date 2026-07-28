#!/usr/bin/env Rscript
# Preregistered secondary models. The Python analysis remains authoritative for
# paired primary contrasts and produces the scored CSV consumed here.

suppressPackageStartupMessages(library(lme4))

args <- commandArgs(trailingOnly = TRUE)
if (length(args) != 2) {
  stop("usage: mixed-models.R scored-runs.csv expert-decisions.csv")
}

runs <- read.csv(args[[1]], stringsAsFactors = TRUE)
primary <- subset(runs, phase == "primary")

quality <- lmer(
  rubric_score ~ arm + domain + difficulty + (1 | task_id) + (1 | seed),
  data = primary
)
completion <- glmer(
  task_complete ~ arm + domain + difficulty + (1 | task_id) + (1 | seed),
  family = binomial(link = "logit"), data = primary
)

decisions <- read.csv(args[[2]], stringsAsFactors = TRUE)
approval_time <- lmer(
  log1p(decision_seconds) ~ arm + domain + (1 | task_id) + (1 | expert_id),
  data = decisions
)
approval_rejection <- glmer(
  rejected ~ arm + domain + (1 | task_id) + (1 | expert_id),
  family = binomial(link = "logit"), data = decisions
)

print(summary(quality))
print(summary(completion))
print(summary(approval_time))
print(summary(approval_rejection))
