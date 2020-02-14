FROM mcr.microsoft.com/windows/nanoserver:1809

ENV CNB_USER_ID=S-1-1-0
ENV CNB_GROUP_ID=S-1-1-0

USER ContainerAdministrator

RUN net users /ADD pack /passwordreq:no /expires:never

# MIXINS?
LABEL io.buildpacks.stack.id=pack.test.stack

USER pack

